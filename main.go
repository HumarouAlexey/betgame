package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed web/*
var webFS embed.FS

// These must match the length of skillTasks / mentalTasks / duelTasks in
// web/index.html. If you add or remove tasks there, update these to match.
const (
	skillTaskCount  = 68
	mentalTaskCount = 68
	duelTaskCount   = 18
	minBetAmount    = 5
	startingMoney   = 100
	// A player whose money drops to rescueThreshold or below after a
	// challenge is topped up by rescueAmount, so nobody is stuck unable to
	// bet. Losing a bet can't take anyone below minBetAmount, so in practice
	// this catches €5–10.
	rescueThreshold = 10
	rescueAmount    = 100
	maxNameLength   = 24
	// A TV counts as connected while it has asked for the game within this
	// many milliseconds; it asks every 1.5 s.
	tvTimeoutMs = 5000
	// maxResultDetail caps what a phone mini-game reports with its result
	// (e.g. "10.27" seconds or "5/7" colours).
	maxResultDetail = 16
)

type Player struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Money    int    `json:"money"`
	Position int    `json:"position"`
}

type Bet struct {
	WillSucceed bool `json:"willSucceed"`
	Amount      int  `json:"amount"`
}

type GameState struct {
	Phase            string      `json:"phase"`
	Players          []Player    `json:"players"`
	CurrentPlayerIdx int         `json:"currentPlayerIndex"`
	DiceValue        *int        `json:"diceValue"`
	CurrentTaskType  *string     `json:"currentTaskType"`
	CurrentTaskIndex *int        `json:"currentTaskIndex"`
	Bets             map[int]Bet `json:"bets"`
	BlindBetTaskType *string     `json:"blindBetTaskType"`
	TaskSuccess      *bool       `json:"taskSuccess"`
	ShowResults      bool        `json:"showResults"`
	// Rescued lists the players the bank topped up after this challenge
	// (see rescueThreshold); cleared at the next turn.
	Rescued         []int  `json:"rescued"`
	UsedSkillTasks  []int  `json:"usedSkillTasks"`
	UsedMentalTasks []int  `json:"usedMentalTasks"`
	UsedDuelTasks   []int  `json:"usedDuelTasks"`
	PerformEndsAt   *int64 `json:"performEndsAt"`
	// Track is the song in a song challenge. The performer gets an empty
	// one until they've answered (see sanitizeState).
	Track *Track `json:"track"`
	// SongRevealed is set when the performer asks to see the song early.
	SongRevealed bool `json:"songRevealed"`
	// MusicAvailable says whether song challenges can be dealt right now
	// (the server needs to reach Deezer).
	MusicAvailable bool `json:"musicAvailable"`
	// DuelOpponent is the player the performer challenged in a duel. In a
	// duel, a bet on success is a bet on the performer winning.
	DuelOpponent *int `json:"duelOpponent"`
	// ResultDetail is what a phone mini-game measured, shown with the
	// results.
	ResultDetail string `json:"resultDetail"`
	BetsCount    int    `json:"betsCount"`
	BetPlacedBy  []int  `json:"betPlacedBy"`
	You          *int   `json:"you"`
	// ServerNow is the server's clock at the moment of the response, so
	// clients can run countdowns against server time rather than their own
	// (possibly skewed) clocks.
	ServerNow int64 `json:"serverNow"`
	// TVConnected says a big screen is showing the game (see handleGetGame);
	// song clips then play there instead of on the performer's phone.
	TVConnected bool `json:"tvConnected"`
	// tvSeenAt is when a TV last asked for the game.
	tvSeenAt int64
}

type Game struct {
	mu    sync.Mutex
	state GameState
	// playerTokens[i] is player i's secret identity token, required as
	// ?playerId= on every request made "as" that player. Never sent to
	// clients as part of GameState — only handed out once, to the phone
	// that joined as that player (see handleJoin).
	playerTokens []string
	// playerLangs[i] is the language player i last had the page in ("en"
	// or "ru"), as sent with their requests; it decides which songs the
	// game gets (see mostlyRussian).
	playerLangs []string
	// typeBag holds the challenge types still to be dealt from the current
	// shuffled bag (see drawTaskType); lastType is the one dealt most recently.
	typeBag  []string
	lastType string
}

// taskTypeBag is one round of challenge types. Dealing from a shuffled bag
// instead of rolling for each turn keeps the five types evenly mixed: no
// type comes up more than twice in a row, and a blind bet is never more
// than sixteen challenges away.
var taskTypeBag = []string{"skill", "skill", "mental", "mental", "music", "music", "duel", "duel", "blind", "blind"}

// drawTaskType deals the next challenge type, refilling and reshuffling the
// bag when it runs out. A fresh bag never opens with the type the previous
// one ended on, so a streak can't straddle two bags. Types in skip (song
// challenges without Deezer, duels with nobody to challenge) are passed over.
func (g *Game) drawTaskType(skip map[string]bool) string {
	for {
		if t := g.drawFromBag(); !skip[t] {
			return t
		}
	}
}

func (g *Game) drawFromBag() string {
	if len(g.typeBag) == 0 {
		bag := append([]string(nil), taskTypeBag...)
		mrand.Shuffle(len(bag), func(i, j int) { bag[i], bag[j] = bag[j], bag[i] })
		if bag[0] == g.lastType {
			for i := 1; i < len(bag); i++ {
				if bag[i] != g.lastType {
					bag[0], bag[i] = bag[i], bag[0]
					break
				}
			}
		}
		g.typeBag = bag
	}
	t := g.typeBag[0]
	g.typeBag = g.typeBag[1:]
	g.lastType = t
	return t
}

// defaultAdminPassword unlocks removing players unless the
// BETGAME_ADMIN_PASSWORD environment variable sets a different one.
const defaultAdminPassword = "BgaGoSeeWin"

// adminPassword is the organizer password in effect, set at startup.
var adminPassword string

func checkAdminPassword(pw string) bool {
	return adminPassword != "" && subtle.ConstantTimeCompare([]byte(pw), []byte(adminPassword)) == 1
}

// requireAdmin writes a 403 and returns false unless pw is the organizer's
// password.
func requireAdmin(w http.ResponseWriter, r *http.Request, pw string) bool {
	if adminPassword == "" {
		writeErrKey(w, r, 403, "adminDisabled")
		return false
	}
	if !checkAdminPassword(pw) {
		writeErrKey(w, r, 403, "wrongPassword")
		return false
	}
	return true
}

var (
	gamesMu sync.Mutex
	games   = map[string]*Game{}
)

func nowMs() int64 { return time.Now().UnixMilli() }

// randomGameID returns a URL-safe base64 id, short enough to fit in a QR
// code but long enough that nobody stumbles into someone else's game.
func randomGameID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomToken(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

// viewerFromRequest resolves the requesting client's own player index (if
// any) from the ?playerId= query parameter. That parameter carries a
// player's long, unguessable secret token (see Game.playerTokens), not a
// raw index — so a request can't claim to be another player just by editing
// a small number in the URL. Returns nil if the token is missing or doesn't
// match any player in this game.
func viewerFromRequest(g *Game, r *http.Request) *int {
	raw := r.URL.Query().Get("playerId")
	if raw == "" {
		return nil
	}
	for i, tok := range g.playerTokens {
		if tok != "" && tok == raw {
			idx := i
			return &idx
		}
	}
	return nil
}

// sanitizeState returns a copy of state safe to send to a given viewer: bets
// belonging to other players are hidden until results are revealed. Only the
// list of who has placed a bet (not the amount/choice) is ever shared early.
func sanitizeState(state GameState, viewerID *int) GameState {
	out := state
	out.BetsCount = len(state.Bets)
	placedBy := make([]int, 0, len(state.Bets))
	for pid := range state.Bets {
		placedBy = append(placedBy, pid)
	}
	out.BetPlacedBy = placedBy
	out.You = viewerID
	out.ServerNow = nowMs()
	out.MusicAvailable = musicAvailable()
	out.TVConnected = state.tvSeenAt > 0 && out.ServerNow-state.tvSeenAt < tvTimeoutMs

	// The performer can't see which song it is until they've answered:
	// they ask to see it, the clip runs out, or the results are in.
	if state.Track != nil && viewerID != nil && *viewerID == state.CurrentPlayerIdx &&
		!state.ShowResults && !state.SongRevealed && (state.PerformEndsAt == nil || out.ServerNow < *state.PerformEndsAt) {
		out.Track = &Track{}
	}

	// A mental task is a puzzle the current player could start solving the
	// moment they read it, so they only get to see it once everyone else
	// has bet.
	if state.Phase == "betting" && state.CurrentTaskType != nil && *state.CurrentTaskType == "mental" &&
		viewerID != nil && *viewerID == state.CurrentPlayerIdx && len(state.Bets) < len(state.Players)-1 {
		out.CurrentTaskIndex = nil
	}

	if state.ShowResults {
		return out
	}
	filtered := map[int]Bet{}
	if viewerID != nil {
		if b, ok := state.Bets[*viewerID]; ok {
			filtered[*viewerID] = b
		}
	}
	out.Bets = filtered
	return out
}

func respondState(w http.ResponseWriter, r *http.Request, g *Game) {
	viewer := viewerFromRequest(g, r)
	if viewer != nil {
		if lang := requestLang(r); lang != "" {
			g.playerLangs[*viewer] = lang
		}
	}
	writeJSON(w, sanitizeState(g.state, viewer))
}

// requestLang is the page language sent as ?lang=, or "" if none.
func requestLang(r *http.Request) string {
	switch lang := r.URL.Query().Get("lang"); lang {
	case "en", "ru":
		return lang
	}
	return ""
}

// mostlyRussian reports whether more than half the players have the page in
// Russian, in which case the game mixes Russian songs in with the rest.
func (g *Game) mostlyRussian() bool {
	ru := 0
	for _, lang := range g.playerLangs {
		if lang == "ru" {
			ru++
		}
	}
	return ru*2 > len(g.playerLangs)
}

// requireCurrentPlayer rejects the request unless it was made by the game's
// current player (identified by the ?playerId= query param). Writes a 403
// and returns false if not; callers should stop handling on false.
func requireCurrentPlayer(w http.ResponseWriter, r *http.Request, g *Game) bool {
	viewerID := viewerFromRequest(g, r)
	if viewerID == nil || *viewerID != g.state.CurrentPlayerIdx {
		writeErrKey(w, r, 403, "onlyCurrentPlayer")
		return false
	}
	return true
}

func pickUnused(total int, used []int) (int, []int) {
	usedSet := map[int]bool{}
	for _, u := range used {
		usedSet[u] = true
	}
	available := make([]int, 0, total)
	for i := 0; i < total; i++ {
		if !usedSet[i] {
			available = append(available, i)
		}
	}
	if len(available) == 0 {
		pick := mrand.Intn(total)
		return pick, []int{pick}
	}
	pick := available[mrand.Intn(len(available))]
	return pick, append(used, pick)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type errMsg struct{ en, ru string }

var errMessages = map[string]errMsg{
	"gameNotFound":       {"game not found", "игра не найдена"},
	"onlyCurrentPlayer":  {"only the current player can do this", "это может сделать только текущий игрок"},
	"invalidType":        {"invalid type", "недопустимый тип"},
	"blindTypeNotChosen": {"blind bet task type not chosen", "тип задания для слепой ставки не выбран"},
	"badRequest":         {"invalid request", "некорректный запрос"},
	"invalidPlayer":      {"invalid player", "недопустимый игрок"},
	"currentPlayerNoBet": {"the current player can't place a bet", "текущий игрок не может делать ставки"},
	"betTooMuch":         {"can't bet more than half your money", "нельзя ставить больше половины своих денег"},
	"methodNotAllowed":   {"method not allowed", "метод не разрешён"},
	"unknownRoute":       {"unknown route", "неизвестный маршрут"},
	"nameRequired":       {"enter your name", "введите своё имя"},
	"nameTaken":          {"that name is already taken", "это имя уже занято"},
	"gameAlreadyOver":    {"this game is already over", "эта игра уже закончилась"},
	"noPlayers":          {"someone has to join before the game can start", "чтобы начать игру, кто-то должен присоединиться"},
	"mustJoinFirst":      {"you must join the game as a player first", "сначала нужно присоединиться к игре"},
	"adminDisabled":      {"removing players is turned off on this server", "удаление игроков отключено на этом сервере"},
	"wrongPassword":      {"wrong password", "неверный пароль"},
	"playerChanged":      {"the player list has changed, try again", "список игроков изменился, попробуйте ещё раз"},
	"musicUnavailable":   {"song challenges need the server to reach Deezer", "для заданий с песнями серверу нужен доступ к Deezer"},
	"noSong":             {"there is no song in play", "сейчас песня не играет"},
	"duelistNoBet":       {"you're in the duel, so you can't bet on it", "ты участвуешь в дуэли, поэтому не можешь на неё ставить"},
	"invalidOpponent":    {"choose another player as your opponent", "выберите соперником другого игрока"},
}

// writeErrKey writes an error message in the language requested via ?lang=ru
// (defaults to English), looking up msg by key from errMessages.
func writeErrKey(w http.ResponseWriter, r *http.Request, code int, key string) {
	m, ok := errMessages[key]
	msg := key
	if ok {
		msg = m.en
		if r.URL.Query().Get("lang") == "ru" {
			msg = m.ru
		}
	}
	writeErr(w, code, msg)
}

func minBetMsg(lang string) string {
	if lang == "ru" {
		return fmt.Sprintf("минимальная ставка — %d", minBetAmount)
	}
	return fmt.Sprintf("must bet at least %d", minBetAmount)
}

func getGame(id string) (*Game, bool) {
	gamesMu.Lock()
	defer gamesMu.Unlock()
	g, ok := games[id]
	return g, ok
}

// ---- HTTP handlers ----

// handleCreateGame opens an empty lobby. Players add themselves through
// handleJoin, and any of them can then start the game.
func handleCreateGame(w http.ResponseWriter, r *http.Request) {
	state := GameState{
		Phase:            "lobby",
		Players:          []Player{},
		CurrentPlayerIdx: 0,
		Bets:             map[int]Bet{},
		UsedSkillTasks:   []int{},
		UsedMentalTasks:  []int{},
		UsedDuelTasks:    []int{},
	}

	gamesMu.Lock()
	var id string
	for {
		id = randomGameID()
		if _, exists := games[id]; !exists {
			break
		}
	}
	games[id] = &Game{
		state:        state,
		playerTokens: []string{},
		playerLangs:  []string{},
	}
	gamesMu.Unlock()

	writeJSON(w, map[string]interface{}{
		"gameId": id,
		"state":  sanitizeState(state, nil),
	})
}

func handleGetGame(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Query().Get("tv") == "1" {
		respondTV(w, g)
		return
	}
	respondState(w, r, g)
}

// respondTV answers a big screen showing the game. It sees what the current
// player sees, hidden song and all, since that player is looking at it too,
// but it isn't anyone, so it can't act as them.
func respondTV(w http.ResponseWriter, g *Game) {
	g.state.tvSeenAt = nowMs()
	var viewer *int
	if len(g.state.Players) > 0 && g.state.Phase != "lobby" && g.state.Phase != "gameOver" {
		cur := g.state.CurrentPlayerIdx
		viewer = &cur
	}
	out := sanitizeState(g.state, viewer)
	out.You = nil
	writeJSON(w, out)
}

// handleStart lets any player who has joined start the game; onlookers who
// haven't joined yet can't.
func handleStart(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if viewerFromRequest(g, r) == nil {
		writeErrKey(w, r, 403, "mustJoinFirst")
		return
	}
	if len(g.state.Players) == 0 {
		writeErrKey(w, r, 400, "noPlayers")
		return
	}
	if g.state.Phase == "lobby" {
		g.state.Phase = "playing"
	}
	respondState(w, r, g)
}

type joinReq struct {
	Name string `json:"name"`
}

// handleJoin adds a new player under the name they typed and hands back
// their secret identity token. It works in the lobby and mid-game alike: a
// late joiner simply takes the last seat in the turn order with the same
// starting money everyone else began with.
func handleJoin(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req joinReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrKey(w, r, 400, "badRequest")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErrKey(w, r, 400, "nameRequired")
		return
	}
	if utf8.RuneCountInString(name) > maxNameLength {
		name = string([]rune(name)[:maxNameLength])
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.Phase == "gameOver" {
		writeErrKey(w, r, 409, "gameAlreadyOver")
		return
	}
	for _, p := range g.state.Players {
		if strings.EqualFold(p.Name, name) {
			writeErrKey(w, r, 409, "nameTaken")
			return
		}
	}
	idx := len(g.state.Players)
	g.state.Players = append(g.state.Players, Player{ID: idx, Name: name, Money: startingMoney, Position: 0})
	token := randomToken(24)
	g.playerTokens = append(g.playerTokens, token)
	g.playerLangs = append(g.playerLangs, requestLang(r))
	writeJSON(w, map[string]interface{}{
		"token": token,
		"state": sanitizeState(g.state, &idx),
	})
}

func handleRoll(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}

	if g.state.DiceValue != nil {
		respondState(w, r, g)
		return
	}

	roll := mrand.Intn(6) + 1
	cp := &g.state.Players[g.state.CurrentPlayerIdx]
	oldPos := cp.Position
	newPos := oldPos + roll
	if newPos/10 > oldPos/10 {
		cp.Money += 50
	}
	cp.Position = newPos

	g.state.Bets = map[int]Bet{}

	if newPos%10 == 0 {
		// landed on START — bonus already applied, roll again
		g.state.DiceValue = nil
		g.state.Phase = "playing"
		respondState(w, r, g)
		return
	}

	// The roll moves the player (and pays out for passing START); which
	// kind of challenge they get comes from the shuffled bag, not the square.
	g.state.DiceValue = &roll
	skip := map[string]bool{"music": !musicAvailable(), "duel": len(g.state.Players) < 2}
	switch t := g.drawTaskType(skip); t {
	case "skill":
		idx, used := pickUnused(skillTaskCount, g.state.UsedSkillTasks)
		g.state.UsedSkillTasks = used
		g.state.CurrentTaskType = &t
		g.state.CurrentTaskIndex = &idx
		g.state.Phase = "betting"
	case "mental":
		idx, used := pickUnused(mentalTaskCount, g.state.UsedMentalTasks)
		g.state.UsedMentalTasks = used
		g.state.CurrentTaskType = &t
		g.state.CurrentTaskIndex = &idx
		g.state.Phase = "betting"
	case "music":
		g.state.CurrentTaskType = &t
		g.state.Track = pickTrack(g.mostlyRussian())
		g.state.Phase = "betting"
	case "duel":
		// The duel is dealt first, so the performer picks an opponent
		// knowing what they'll compete at.
		idx, used := pickUnused(duelTaskCount, g.state.UsedDuelTasks)
		g.state.UsedDuelTasks = used
		g.state.CurrentTaskType = &t
		g.state.CurrentTaskIndex = &idx
		g.state.Phase = "duelPick"
	default:
		g.state.Phase = "blindBet"
	}

	respondState(w, r, g)
}

type blindTypeReq struct {
	Type string `json:"type"`
}

func handleBlindType(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req blindTypeReq
	json.NewDecoder(r.Body).Decode(&req)
	if req.Type != "skill" && req.Type != "mental" && req.Type != "music" {
		writeErrKey(w, r, 400, "invalidType")
		return
	}
	if req.Type == "music" && !musicAvailable() {
		writeErrKey(w, r, 503, "musicUnavailable")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}
	g.state.BlindBetTaskType = &req.Type
	g.state.Phase = "blindBetting"
	g.state.Bets = map[int]Bet{}
	respondState(w, r, g)
}

func handleReveal(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}
	if g.state.BlindBetTaskType == nil {
		writeErrKey(w, r, 400, "blindTypeNotChosen")
		return
	}
	var idx int
	t := *g.state.BlindBetTaskType
	switch t {
	case "skill":
		idx, g.state.UsedSkillTasks = pickUnused(skillTaskCount, g.state.UsedSkillTasks)
		g.state.CurrentTaskIndex = &idx
	case "mental":
		idx, g.state.UsedMentalTasks = pickUnused(mentalTaskCount, g.state.UsedMentalTasks)
		g.state.CurrentTaskIndex = &idx
	case "music":
		g.state.Track = pickTrack(g.mostlyRussian())
	}
	g.state.CurrentTaskType = &t
	g.state.Phase = "revealBlindTask"
	respondState(w, r, g)
}

type duelOpponentReq struct {
	// PlayerID and Name pick the opponent; the name guards against the list
	// having shifted since the performer looked at it.
	PlayerID int    `json:"playerId"`
	Name     string `json:"name"`
}

func handleDuelOpponent(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req duelOpponentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrKey(w, r, 400, "badRequest")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}
	if g.state.Phase != "duelPick" {
		respondState(w, r, g)
		return
	}
	if req.PlayerID < 0 || req.PlayerID >= len(g.state.Players) || req.PlayerID == g.state.CurrentPlayerIdx {
		writeErrKey(w, r, 400, "invalidOpponent")
		return
	}
	if g.state.Players[req.PlayerID].Name != req.Name {
		writeErrKey(w, r, 409, "playerChanged")
		return
	}
	opp := req.PlayerID
	g.state.DuelOpponent = &opp
	g.state.Bets = map[int]Bet{}
	g.state.Phase = "betting"
	respondState(w, r, g)
}

type startChallengeReq struct {
	TaskTimeSeconds int `json:"taskTimeSeconds"`
	// NoTimeLimit runs the challenge without a deadline, for tasks whose
	// outcome is decided by a number of tries rather than by a clock.
	NoTimeLimit bool `json:"noTimeLimit"`
}

func handleStartChallenge(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req startChallengeReq
	json.NewDecoder(r.Body).Decode(&req)
	if req.TaskTimeSeconds <= 0 {
		req.TaskTimeSeconds = 30
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}
	// A song challenge lasts exactly as long as the clip.
	if g.state.Track != nil {
		req.NoTimeLimit, req.TaskTimeSeconds = false, songSeconds
	}
	if req.NoTimeLimit {
		g.state.PerformEndsAt = nil
	} else {
		ends := nowMs() + int64(req.TaskTimeSeconds)*1000
		g.state.PerformEndsAt = &ends
	}
	g.state.Phase = "performing"
	respondState(w, r, g)
}

type betReq struct {
	WillSucceed bool `json:"willSucceed"`
	Amount      int  `json:"amount"`
}

func handleBet(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req betReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrKey(w, r, 400, "badRequest")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	// The bettor's identity comes only from their own secret token (?playerId=),
	// never from the request body — otherwise anyone could bet as anyone else
	// just by naming a different playerId in the JSON.
	viewerID := viewerFromRequest(g, r)
	if viewerID == nil {
		writeErrKey(w, r, 403, "mustJoinFirst")
		return
	}
	playerID := *viewerID
	if playerID == g.state.CurrentPlayerIdx {
		writeErrKey(w, r, 400, "currentPlayerNoBet")
		return
	}
	if g.state.DuelOpponent != nil && playerID == *g.state.DuelOpponent {
		writeErrKey(w, r, 400, "duelistNoBet")
		return
	}
	player := g.state.Players[playerID]
	if req.Amount < minBetAmount {
		writeErr(w, 400, minBetMsg(r.URL.Query().Get("lang")))
		return
	}
	if req.Amount > player.Money/2 {
		writeErrKey(w, r, 400, "betTooMuch")
		return
	}
	g.state.Bets[playerID] = Bet{WillSucceed: req.WillSucceed, Amount: req.Amount}
	respondState(w, r, g)
}

type resultReq struct {
	// Success means the performer did it, or in a duel, won it.
	Success bool `json:"success"`
	// Detail is what a phone mini-game measured, if anything.
	Detail string `json:"detail"`
}

func handleResult(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req resultReq
	json.NewDecoder(r.Body).Decode(&req)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}

	if g.state.TaskSuccess != nil {
		respondState(w, r, g)
		return
	}
	total := 0
	for pid, bet := range g.state.Bets {
		total += bet.Amount
		if bet.WillSucceed == req.Success {
			p := &g.state.Players[pid]
			p.Money += bet.Amount
		} else {
			p := &g.state.Players[pid]
			p.Money -= bet.Amount
		}
	}
	// The pot goes to whoever came out on top: the performer, or the
	// opponent who beat them in a duel.
	if req.Success {
		g.state.Players[g.state.CurrentPlayerIdx].Money += total
	} else if g.state.DuelOpponent != nil {
		g.state.Players[*g.state.DuelOpponent].Money += total
	}
	detail := strings.TrimSpace(req.Detail)
	if utf8.RuneCountInString(detail) > maxResultDetail {
		detail = string([]rune(detail)[:maxResultDetail])
	}
	g.state.ResultDetail = detail
	g.state.Rescued = []int{}
	for i := range g.state.Players {
		if g.state.Players[i].Money <= rescueThreshold {
			g.state.Players[i].Money += rescueAmount
			g.state.Rescued = append(g.state.Rescued, i)
		}
	}
	success := req.Success
	g.state.TaskSuccess = &success
	g.state.ShowResults = true
	respondState(w, r, g)
}

func handleNextTurn(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}

	g.startTurn((g.state.CurrentPlayerIdx + 1) % len(g.state.Players))
	respondState(w, r, g)
}

// startTurn hands the turn to player idx with a clean slate.
func (g *Game) startTurn(idx int) {
	g.state.CurrentPlayerIdx = idx
	g.state.DiceValue = nil
	g.state.CurrentTaskType = nil
	g.state.CurrentTaskIndex = nil
	g.state.Bets = map[int]Bet{}
	g.state.ShowResults = false
	g.state.TaskSuccess = nil
	g.state.Rescued = nil
	g.state.Phase = "playing"
	g.state.BlindBetTaskType = nil
	g.state.PerformEndsAt = nil
	g.state.Track = nil
	g.state.SongRevealed = false
	g.state.DuelOpponent = nil
	g.state.ResultDetail = ""
}

// removePlayer takes player k out of the game. Players are addressed by
// their index everywhere (IDs, tokens, bets, the current turn), so every
// index above k shifts down by one. If k was mid-turn, the turn passes to
// whoever now sits in that seat.
func (g *Game) removePlayer(k int) {
	s := &g.state
	s.Players = append(s.Players[:k], s.Players[k+1:]...)
	for i := range s.Players {
		s.Players[i].ID = i
	}
	g.playerTokens = append(g.playerTokens[:k], g.playerTokens[k+1:]...)
	g.playerLangs = append(g.playerLangs[:k], g.playerLangs[k+1:]...)

	shift := func(i int) int {
		if i > k {
			return i - 1
		}
		return i
	}
	bets := map[int]Bet{}
	for pid, b := range s.Bets {
		if pid != k {
			bets[shift(pid)] = b
		}
	}
	s.Bets = bets
	if s.Rescued != nil {
		rescued := []int{}
		for _, pid := range s.Rescued {
			if pid != k {
				rescued = append(rescued, shift(pid))
			}
		}
		s.Rescued = rescued
	}

	// Losing the opponent sends the performer back to pick another one.
	if s.DuelOpponent != nil {
		if *s.DuelOpponent == k {
			s.DuelOpponent = nil
			s.Bets = map[int]Bet{}
			if s.Phase == "betting" || s.Phase == "performing" {
				s.Phase = "duelPick"
				s.PerformEndsAt = nil
			}
		} else {
			opp := shift(*s.DuelOpponent)
			s.DuelOpponent = &opp
		}
	}

	switch {
	case len(s.Players) == 0:
		s.CurrentPlayerIdx = 0
		if s.Phase != "lobby" {
			s.Phase = "gameOver"
		}
	case s.Phase == "lobby" || s.Phase == "gameOver":
		s.CurrentPlayerIdx = shift(s.CurrentPlayerIdx) % len(s.Players)
	case k == s.CurrentPlayerIdx:
		g.startTurn(k % len(s.Players))
	case k < s.CurrentPlayerIdx:
		s.CurrentPlayerIdx--
	}
	// A duel with nobody left to challenge is called off.
	if s.Phase == "duelPick" && len(s.Players) < 2 {
		g.startTurn(s.CurrentPlayerIdx)
	}
}

type adminReq struct {
	Password string `json:"password"`
	// PlayerID and Name pick the player to remove. The name guards against
	// the list having shifted since the organizer looked at it.
	PlayerID int    `json:"playerId"`
	Name     string `json:"name"`
}

// handleAdminCheck lets the client verify the organizer's password before
// showing the remove buttons.
func handleAdminCheck(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := getGame(id); !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req adminReq
	json.NewDecoder(r.Body).Decode(&req)
	if !requireAdmin(w, r, req.Password) {
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func handleKick(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req adminReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrKey(w, r, 400, "badRequest")
		return
	}
	if !requireAdmin(w, r, req.Password) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.PlayerID < 0 || req.PlayerID >= len(g.state.Players) || g.state.Players[req.PlayerID].Name != req.Name {
		writeErrKey(w, r, 409, "playerChanged")
		return
	}
	g.removePlayer(req.PlayerID)
	respondState(w, r, g)
}

func handleEnd(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g) {
		return
	}
	g.state.Phase = "gameOver"
	respondState(w, r, g)
}

// routes a request like /api/games/{id}/{action}
func apiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := r.URL.Path[len("/api/games/"):]
	if path == "" {
		if r.Method == http.MethodPost {
			handleCreateGame(w, r)
			return
		}
		writeErrKey(w, r, 405, "methodNotAllowed")
		return
	}

	// split id and optional action
	id := path
	action := ""
	for i, c := range path {
		if c == '/' {
			id = path[:i]
			action = path[i+1:]
			break
		}
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		handleGetGame(w, r, id)
	case action == "start" && r.Method == http.MethodPost:
		handleStart(w, r, id)
	case action == "join" && r.Method == http.MethodPost:
		handleJoin(w, r, id)
	case action == "roll" && r.Method == http.MethodPost:
		handleRoll(w, r, id)
	case action == "blindType" && r.Method == http.MethodPost:
		handleBlindType(w, r, id)
	case action == "reveal" && r.Method == http.MethodPost:
		handleReveal(w, r, id)
	case action == "duelOpponent" && r.Method == http.MethodPost:
		handleDuelOpponent(w, r, id)
	case action == "startChallenge" && r.Method == http.MethodPost:
		handleStartChallenge(w, r, id)
	case action == "bet" && r.Method == http.MethodPost:
		handleBet(w, r, id)
	case action == "result" && r.Method == http.MethodPost:
		handleResult(w, r, id)
	case action == "nextTurn" && r.Method == http.MethodPost:
		handleNextTurn(w, r, id)
	case action == "end" && r.Method == http.MethodPost:
		handleEnd(w, r, id)
	case action == "adminCheck" && r.Method == http.MethodPost:
		handleAdminCheck(w, r, id)
	case action == "kick" && r.Method == http.MethodPost:
		handleKick(w, r, id)
	case action == "songPreview" && r.Method == http.MethodGet:
		handleSongPreview(w, r, id)
	case action == "revealSong" && r.Method == http.MethodPost:
		handleRevealSong(w, r, id)
	default:
		writeErrKey(w, r, 404, "unknownRoute")
	}
}

func localIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipNet.IP.To4() != nil {
			out = append(out, ipNet.IP.String())
		}
	}
	return out
}

func main() {
	port := "8080"
	adminPassword = os.Getenv("BETGAME_ADMIN_PASSWORD")
	if adminPassword == "" {
		adminPassword = defaultAdminPassword
	}
	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal("не найдены встроенные файлы web/: ", err)
	}

	loadPlayedTracks()
	go keepTracksFresh()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/games", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleCreateGame(w, r)
			return
		}
		writeErrKey(w, r, 405, "methodNotAllowed")
	})
	mux.HandleFunc("/api/games/", apiHandler)
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	addr := ":" + port
	log.Printf("Сервер «Спорим, не сможешь!» запущен на порту %s", port)
	log.Printf("На этом устройстве:  http://localhost:%s", port)
	for _, ip := range localIPs() {
		log.Printf("В сети Wi-Fi:        http://%s:%s", ip, port)
	}
	if os.Getenv("BETGAME_ADMIN_PASSWORD") == "" {
		log.Printf("Удаление игроков: пароль по умолчанию из main.go")
	} else {
		log.Printf("Удаление игроков: пароль из BETGAME_ADMIN_PASSWORD")
	}
	log.Fatal(http.ListenAndServe(addr, mux))
}
