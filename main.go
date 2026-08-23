package main

import (
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

// These must match the length of skillTasks / mentalTasks in web/index.html.
// If you add or remove tasks there, update these two numbers to match.
const (
	skillTaskCount  = 72
	mentalTaskCount = 81
	minBetAmount    = 5
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
	UsedSkillTasks   []int       `json:"usedSkillTasks"`
	UsedMentalTasks  []int       `json:"usedMentalTasks"`
	GameTimeMinutes  int         `json:"gameTimeMinutes"`
	GameEndsAt       *int64      `json:"gameEndsAt"`
	PerformEndsAt    *int64      `json:"performEndsAt"`
	BetsCount        int         `json:"betsCount"`
	BetPlacedBy      []int       `json:"betPlacedBy"`
}

type Game struct {
	mu         sync.Mutex
	state      GameState
	adminToken string
}

var (
	gamesMu sync.Mutex
	games   = map[string]*Game{}
)

func nowMs() int64 { return time.Now().UnixMilli() }

func randomGameID() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
	b := make([]byte, 5)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
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

// viewerFromRequest extracts the requesting client's own player id (if any)
// from the ?playerId= query parameter, used to decide which bets they may see.
func viewerFromRequest(r *http.Request) *int {
	raw := r.URL.Query().Get("playerId")
	if raw == "" {
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &n
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

func respondState(w http.ResponseWriter, r *http.Request, state GameState) {
	writeJSON(w, sanitizeState(state, viewerFromRequest(r)))
}

// requireCurrentPlayer rejects the request unless it was made by the game's
// current player (identified by the ?playerId= query param). Writes a 403
// and returns false if not; callers should stop handling on false.
func requireCurrentPlayer(w http.ResponseWriter, r *http.Request, state GameState) bool {
	viewerID := viewerFromRequest(r)
	if viewerID == nil || *viewerID != state.CurrentPlayerIdx {
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
	"badBody":            {"bad request body", "некорректное тело запроса"},
	"badNumPlayers":      {"invalid number of players", "недопустимое количество игроков"},
	"onlyHostCanStart":   {"only the host can start the game", "начать игру может только хост"},
	"onlyCurrentPlayer":  {"only the current player can do this", "это может сделать только текущий игрок"},
	"invalidType":        {"invalid type", "недопустимый тип"},
	"blindTypeNotChosen": {"blind bet task type not chosen", "тип задания для слепой ставки не выбран"},
	"badRequest":         {"invalid request", "некорректный запрос"},
	"invalidPlayer":      {"invalid player", "недопустимый игрок"},
	"currentPlayerNoBet": {"the current player can't place a bet", "текущий игрок не может делать ставки"},
	"betTooMuch":         {"can't bet more than half your money", "нельзя ставить больше половины своих денег"},
	"methodNotAllowed":   {"method not allowed", "метод не разрешён"},
	"unknownRoute":       {"unknown route", "неизвестный маршрут"},
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

type createGameReq struct {
	NumPlayers      int      `json:"numPlayers"`
	GameTimeMinutes int      `json:"gameTimeMinutes"`
	PlayerNames     []string `json:"playerNames"`
}

func handleCreateGame(w http.ResponseWriter, r *http.Request) {
	var req createGameReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrKey(w, r, 400, "badBody")
		return
	}
	if req.NumPlayers < 2 || req.NumPlayers > 12 {
		writeErrKey(w, r, 400, "badNumPlayers")
		return
	}
	if req.GameTimeMinutes <= 0 {
		req.GameTimeMinutes = 30
	}

	players := make([]Player, req.NumPlayers)
	for i := 0; i < req.NumPlayers; i++ {
		name := ""
		if i < len(req.PlayerNames) {
			name = req.PlayerNames[i]
		}
		if name == "" {
			if r.URL.Query().Get("lang") == "ru" {
				name = fmt.Sprintf("Игрок %d", i+1)
			} else {
				name = fmt.Sprintf("Player %d", i+1)
			}
		}
		players[i] = Player{ID: i, Name: name, Money: 100, Position: 0}
	}

	state := GameState{
		Phase:            "lobby",
		Players:          players,
		CurrentPlayerIdx: 0,
		Bets:             map[int]Bet{},
		UsedSkillTasks:   []int{},
		UsedMentalTasks:  []int{},
		GameTimeMinutes:  req.GameTimeMinutes,
	}

	adminToken := randomToken(24)

	gamesMu.Lock()
	var id string
	for {
		id = randomGameID()
		if _, exists := games[id]; !exists {
			break
		}
	}
	games[id] = &Game{state: state, adminToken: adminToken}
	gamesMu.Unlock()

	writeJSON(w, map[string]interface{}{
		"gameId":     id,
		"adminToken": adminToken,
		"state":      sanitizeState(state, nil),
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
	respondState(w, r, g.state)
}

type startReq struct {
	AdminToken string `json:"adminToken"`
}

func handleStart(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	var req startReq
	json.NewDecoder(r.Body).Decode(&req)
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.AdminToken == "" || req.AdminToken != g.adminToken {
		writeErrKey(w, r, 403, "onlyHostCanStart")
		return
	}
	if g.state.Phase == "lobby" {
		g.state.Phase = "playing"
		ends := nowMs() + int64(g.state.GameTimeMinutes)*60000
		g.state.GameEndsAt = &ends
	}
	respondState(w, r, g.state)
}

func handleRoll(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g.state) {
		return
	}

	if g.state.DiceValue != nil {
		respondState(w, r, g.state)
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

	spaceType := newPos % 10
	g.state.Bets = map[int]Bet{}

	switch {
	case spaceType == 0:
		// landed on START — bonus already applied, roll again
		g.state.DiceValue = nil
		g.state.Phase = "playing"
	case spaceType >= 1 && spaceType <= 3:
		idx, used := pickUnused(skillTaskCount, g.state.UsedSkillTasks)
		g.state.UsedSkillTasks = used
		t := "skill"
		g.state.CurrentTaskType = &t
		g.state.CurrentTaskIndex = &idx
		g.state.Phase = "betting"
		g.state.DiceValue = &roll
	case spaceType >= 4 && spaceType <= 6:
		idx, used := pickUnused(mentalTaskCount, g.state.UsedMentalTasks)
		g.state.UsedMentalTasks = used
		t := "mental"
		g.state.CurrentTaskType = &t
		g.state.CurrentTaskIndex = &idx
		g.state.Phase = "betting"
		g.state.DiceValue = &roll
	default: // 7,8,9
		g.state.Phase = "blindBet"
		g.state.DiceValue = &roll
	}

	respondState(w, r, g.state)
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
	if req.Type != "skill" && req.Type != "mental" {
		writeErrKey(w, r, 400, "invalidType")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.BlindBetTaskType = &req.Type
	g.state.Phase = "blindBetting"
	g.state.Bets = map[int]Bet{}
	respondState(w, r, g.state)
}

func handleReveal(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g.state) {
		return
	}
	if g.state.BlindBetTaskType == nil {
		writeErrKey(w, r, 400, "blindTypeNotChosen")
		return
	}
	var idx int
	if *g.state.BlindBetTaskType == "skill" {
		idx, g.state.UsedSkillTasks = pickUnused(skillTaskCount, g.state.UsedSkillTasks)
	} else {
		idx, g.state.UsedMentalTasks = pickUnused(mentalTaskCount, g.state.UsedMentalTasks)
	}
	t := *g.state.BlindBetTaskType
	g.state.CurrentTaskType = &t
	g.state.CurrentTaskIndex = &idx
	g.state.Phase = "revealBlindTask"
	respondState(w, r, g.state)
}

type startChallengeReq struct {
	TaskTimeSeconds int `json:"taskTimeSeconds"`
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
	if !requireCurrentPlayer(w, r, g.state) {
		return
	}
	ends := nowMs() + int64(req.TaskTimeSeconds)*1000
	g.state.PerformEndsAt = &ends
	g.state.Phase = "performing"
	respondState(w, r, g.state)
}

type betReq struct {
	PlayerID    int  `json:"playerId"`
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

	if req.PlayerID < 0 || req.PlayerID >= len(g.state.Players) {
		writeErrKey(w, r, 400, "invalidPlayer")
		return
	}
	if req.PlayerID == g.state.CurrentPlayerIdx {
		writeErrKey(w, r, 400, "currentPlayerNoBet")
		return
	}
	player := g.state.Players[req.PlayerID]
	if req.Amount < minBetAmount {
		writeErr(w, 400, minBetMsg(r.URL.Query().Get("lang")))
		return
	}
	if req.Amount > player.Money/2 {
		writeErrKey(w, r, 400, "betTooMuch")
		return
	}
	g.state.Bets[req.PlayerID] = Bet{WillSucceed: req.WillSucceed, Amount: req.Amount}
	respondState(w, r, g.state)
}

type resultReq struct {
	Success bool `json:"success"`
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

	if g.state.TaskSuccess != nil {
		respondState(w, r, g.state)
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
	if req.Success {
		g.state.Players[g.state.CurrentPlayerIdx].Money += total
	}
	success := req.Success
	g.state.TaskSuccess = &success
	g.state.ShowResults = true
	respondState(w, r, g.state)
}

func handleNextTurn(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g.state) {
		return
	}

	g.state.CurrentPlayerIdx = (g.state.CurrentPlayerIdx + 1) % len(g.state.Players)
	g.state.DiceValue = nil
	g.state.CurrentTaskType = nil
	g.state.CurrentTaskIndex = nil
	g.state.Bets = map[int]Bet{}
	g.state.ShowResults = false
	g.state.TaskSuccess = nil
	g.state.Phase = "playing"
	g.state.BlindBetTaskType = nil
	g.state.PerformEndsAt = nil
	respondState(w, r, g.state)
}

func handleEnd(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !requireCurrentPlayer(w, r, g.state) {
		return
	}
	g.state.Phase = "gameOver"
	respondState(w, r, g.state)
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
	case action == "roll" && r.Method == http.MethodPost:
		handleRoll(w, r, id)
	case action == "blindType" && r.Method == http.MethodPost:
		handleBlindType(w, r, id)
	case action == "reveal" && r.Method == http.MethodPost:
		handleReveal(w, r, id)
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
	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal("не найдены встроенные файлы web/: ", err)
	}

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
	log.Fatal(http.ListenAndServe(addr, mux))
}
