package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Song challenges play a 30-second preview from Deezer's public API, which
// needs no key. Songs come from two pools, refreshed in the background so
// dealing a song never waits on the network:
//   - international hits: Deezer's chart (mostly English), or the playlist
//     named by BETGAME_DEEZER_PLAYLIST;
//   - Russian hits: a few public playlists, or the comma-separated ones in
//     BETGAME_DEEZER_RU_PLAYLISTS.
// A game where most players have the page in Russian gets both pools, half
// and half; any other game gets only the international one. Deezer's preview links
// expire after about 15 minutes, so they are never stored: the client plays
// /songPreview, which looks up a fresh link and redirects to it.

const (
	deezerAPI = "https://api.deezer.com"
	// songSeconds is the length of a Deezer preview, and so the time limit
	// of a song challenge.
	songSeconds          = 30
	trackRefreshInterval = 30 * time.Minute
	trackRetryInterval   = 2 * time.Minute
)

// Track is the song in a song challenge, as sent to clients.
type Track struct {
	ID     int64  `json:"id,omitempty"`
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Cover  string `json:"cover,omitempty"`
}

const (
	poolEN = "en"
	poolRU = "ru"
)

// defaultRussianPlaylists are public Deezer playlists: Russian hits of the
// 2000s, Russian hits of 2025, and Russian rock.
var defaultRussianPlaylists = []string{"11037907182", "13750793941", "3459300842"}

var (
	tracksMu sync.Mutex
	// pools maps poolEN / poolRU to its tracks. A pool is replaced only by
	// a non-empty list, so once song challenges are available they stay
	// available.
	pools = map[string][]Track{}
)

var deezerClient = &http.Client{Timeout: 10 * time.Second}

type deezerTrack struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Preview string `json:"preview"`
	// Readable is missing from chart entries, so only an explicit false
	// rules a track out.
	Readable *bool `json:"readable"`
	Artist   struct {
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		Cover string `json:"cover_medium"`
	} `json:"album"`
}

type deezerError struct {
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// deezerGet decodes the JSON at path into v. Deezer reports most errors as
// a 200 response with an "error" object, so that is checked too.
func deezerGet(path string, v interface{}) error {
	resp, err := deezerClient.Get(deezerAPI + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deezer: %s", resp.Status)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	var de deezerError
	if json.Unmarshal(raw, &de) == nil && de.Error != nil {
		return fmt.Errorf("deezer: %s", de.Error.Message)
	}
	return json.Unmarshal(raw, v)
}

func playlistPath(id string) string {
	return "/playlist/" + url.PathEscape(id) + "/tracks?limit=1000"
}

// poolSources lists the Deezer API paths each pool is filled from.
func poolSources() map[string][]string {
	en := []string{"/chart/0/tracks?limit=100"}
	if id := strings.TrimSpace(os.Getenv("BETGAME_DEEZER_PLAYLIST")); id != "" {
		en = []string{playlistPath(id)}
	}
	ruIDs := defaultRussianPlaylists
	if v := os.Getenv("BETGAME_DEEZER_RU_PLAYLISTS"); strings.TrimSpace(v) != "" {
		ruIDs = nil
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ruIDs = append(ruIDs, id)
			}
		}
	}
	ru := make([]string, len(ruIDs))
	for i, id := range ruIDs {
		ru[i] = playlistPath(id)
	}
	return map[string][]string{poolEN: en, poolRU: ru}
}

// fetchTracks returns the playable tracks at a chart or playlist path.
func fetchTracks(path string) ([]Track, error) {
	var resp struct {
		Data []deezerTrack `json:"data"`
	}
	if err := deezerGet(path, &resp); err != nil {
		return nil, err
	}
	list := make([]Track, 0, len(resp.Data))
	for _, d := range resp.Data {
		if (d.Readable == nil || *d.Readable) && d.Preview != "" {
			list = append(list, Track{ID: d.ID, Title: d.Title, Artist: d.Artist.Name, Cover: d.Album.Cover})
		}
	}
	return list, nil
}

// refreshTracks refills every pool. A source that fails is reported, and
// the pool keeps its previous tracks if none of its sources answered.
func refreshTracks() error {
	var errs []error
	for pool, paths := range poolSources() {
		seen := map[int64]bool{}
		var list []Track
		for _, path := range paths {
			tracks, err := fetchTracks(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
				continue
			}
			for _, t := range tracks {
				if !seen[t.ID] {
					seen[t.ID] = true
					list = append(list, t)
				}
			}
		}
		if len(list) == 0 {
			errs = append(errs, fmt.Errorf("%s pool: no playable tracks", pool))
			continue
		}
		tracksMu.Lock()
		pools[pool] = list
		tracksMu.Unlock()
	}
	return errors.Join(errs...)
}

// keepTracksFresh runs for the life of the server. Without internet access
// the game still works, just without song challenges; without the Russian
// pool, every game gets international songs only.
func keepTracksFresh() {
	failing := false
	for {
		err := refreshTracks()
		switch {
		case err != nil && !failing:
			log.Printf("Не все песни загрузились с Deezer (%v), пробую снова каждые %v", err, trackRetryInterval)
			failing = true
		case err == nil && failing:
			log.Printf("Песни с Deezer снова загружаются")
			failing = false
		}
		if failing {
			time.Sleep(trackRetryInterval)
		} else {
			time.Sleep(trackRefreshInterval)
		}
	}
}

func musicAvailable() bool {
	tracksMu.Lock()
	defer tracksMu.Unlock()
	return len(pools[poolEN]) > 0
}

// played holds the Deezer ids of every song dealt on this server, across
// all games, so a song doesn't come back until the whole list has been
// heard. It is saved to playedTracksFile, so a restart doesn't forget it.
// Guarded by tracksMu.
var played = map[int64]bool{}

// playedTracksFile is set at startup to a file next to the binary.
var playedTracksFile string

func loadPlayedTracks() {
	exe, err := os.Executable()
	if err != nil {
		exe = "betgame"
	}
	playedTracksFile = filepath.Join(filepath.Dir(exe), "played_tracks.json")
	data, err := os.ReadFile(playedTracksFile)
	if err != nil {
		return
	}
	var ids []int64
	if err := json.Unmarshal(data, &ids); err != nil {
		log.Printf("Не удалось прочитать %s: %v", playedTracksFile, err)
		return
	}
	for _, id := range ids {
		played[id] = true
	}
}

// savePlayedTracks writes the played set to a temporary file and renames
// it into place, so a crash mid-write can't leave a broken file.
func savePlayedTracks() {
	ids := make([]int64, 0, len(played))
	for id := range played {
		ids = append(ids, id)
	}
	data, _ := json.Marshal(ids)
	tmp := playedTracksFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("Не удалось сохранить сыгранные песни: %v", err)
		return
	}
	if err := os.Rename(tmp, playedTracksFile); err != nil {
		log.Printf("Не удалось сохранить сыгранные песни: %v", err)
	}
}

// pickTrack deals a song nobody on this server has heard yet. With
// withRussian, it comes from the Russian pool half the time. Once every
// song in a pool has been played, that pool starts over. Callers check
// musicAvailable first.
func pickTrack(withRussian bool) *Track {
	tracksMu.Lock()
	defer tracksMu.Unlock()
	tracks := pools[poolEN]
	if withRussian && len(pools[poolRU]) > 0 && mrand.Intn(2) == 0 {
		tracks = pools[poolRU]
	}
	available := make([]Track, 0, len(tracks))
	for _, t := range tracks {
		if !played[t.ID] {
			available = append(available, t)
		}
	}
	if len(available) == 0 {
		for _, t := range tracks {
			delete(played, t.ID)
		}
		available = tracks
	}
	t := available[mrand.Intn(len(available))]
	played[t.ID] = true
	savePlayedTracks()
	return &t
}

func previewURL(id int64) (string, error) {
	var d deezerTrack
	if err := deezerGet(fmt.Sprintf("/track/%d", id), &d); err != nil {
		return "", err
	}
	if d.Preview == "" {
		return "", errors.New("deezer: track has no preview")
	}
	return d.Preview, nil
}

// handleSongPreview redirects to a fresh preview of the current song, so an
// <audio> element can simply point here.
func handleSongPreview(w http.ResponseWriter, r *http.Request, id string) {
	g, ok := getGame(id)
	if !ok {
		writeErrKey(w, r, 404, "gameNotFound")
		return
	}
	g.mu.Lock()
	var trackID int64
	if g.state.Track != nil {
		trackID = g.state.Track.ID
	}
	g.mu.Unlock()
	if trackID == 0 {
		writeErrKey(w, r, 404, "noSong")
		return
	}
	u, err := previewURL(trackID)
	if err != nil {
		log.Printf("Не удалось получить отрывок трека %d: %v", trackID, err)
		writeErrKey(w, r, 502, "musicUnavailable")
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

// handleRevealSong shows the performer the song once they've given their
// answer, so they can judge it.
func handleRevealSong(w http.ResponseWriter, r *http.Request, id string) {
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
	if g.state.Track != nil && g.state.Phase == "performing" {
		g.state.SongRevealed = true
	}
	respondState(w, r, g)
}
