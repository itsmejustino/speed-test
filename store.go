package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ------------------------------------------------------------------ Settings

type Settings struct {
	Server           string  `json:"server"`           // speed test server
	DirectionSeconds float64 `json:"directionSeconds"` // length of the download phase, and of the upload phase
	IdleSeconds      float64 `json:"idleSeconds"`      // quiet line at the start: idle latency, jitter and loss
	WarmupSeconds    float64 `json:"warmupSeconds"`    // left out of each phase while TCP ramps up
	Streams          int     `json:"streams"`          // TCP connections in each direction
	ProbeRate        int     `json:"probeRate"`        // pings per second, all through the test
	RadarToken       string  `json:"radarToken"`       // Cloudflare API token for the comparison with other users
}

// Sent back to the page instead of the token itself.
const keepToken = "__keep__"

func defaultSettings() Settings {
	return Settings{
		Server:           "https://speed.cloudflare.com",
		DirectionSeconds: 15,
		IdleSeconds:      4,
		WarmupSeconds:    2.5,
		Streams:          6,
		ProbeRate:        20,
	}
}

func clampF(v, lo, hi, def float64) float64 {
	if v != v || v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampI(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (s *Settings) sanitize() {
	d := defaultSettings()
	s.Server = strings.TrimRight(strings.TrimSpace(s.Server), "/")
	if !strings.HasPrefix(s.Server, "https://") && !strings.HasPrefix(s.Server, "http://") {
		s.Server = d.Server
	}
	s.DirectionSeconds = clampF(s.DirectionSeconds, 5, 30, d.DirectionSeconds)
	s.IdleSeconds = clampF(s.IdleSeconds, 2, 15, d.IdleSeconds)
	s.WarmupSeconds = clampF(s.WarmupSeconds, 0.5, 6, d.WarmupSeconds)
	if s.WarmupSeconds > s.DirectionSeconds/2 {
		s.WarmupSeconds = s.DirectionSeconds / 2
	}
	s.Streams = clampI(s.Streams, 1, 16, d.Streams)
	s.ProbeRate = clampI(s.ProbeRate, 2, 50, d.ProbeRate)
	s.RadarToken = cleanToken(s.RadarToken)
}

// People paste tokens in many shapes: with "Bearer " in front, inside quotes,
// as a whole curl line, with a line break. Keep only the token itself.
func cleanToken(t string) string {
	t = strings.TrimSpace(t)
	if t == keepToken {
		return t
	}
	if i := strings.LastIndex(strings.ToLower(t), "bearer"); i >= 0 {
		t = t[i+len("bearer"):]
	}
	t = strings.TrimLeft(t, " :\t\r\n\"'")
	if f := strings.Fields(t); len(f) > 0 {
		t = f[0]
	} else {
		t = ""
	}
	return strings.Trim(t, "\"'`;,")
}

// What the page sees: the token is replaced by a marker so it never leaves this program.
func (s Settings) public() map[string]any {
	tok := ""
	if s.RadarToken != "" {
		tok = keepToken
	}
	return map[string]any{
		"server":           s.Server,
		"directionSeconds": s.DirectionSeconds,
		"idleSeconds":      s.IdleSeconds,
		"warmupSeconds":    s.WarmupSeconds,
		"streams":          s.Streams,
		"probeRate":        s.ProbeRate,
		"radarToken":       tok,
		"defaults":         defaultSettings(),
	}
}

func loadSettings() Settings {
	s := defaultSettings()
	path := filepath.Join(baseDir, "settings.json")
	migrate := false
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			keepDamaged(path)
			s = defaultSettings()
		}
	}
	if isProtected(s.RadarToken) {
		plain, err := unprotectSecret(s.RadarToken)
		if err != nil {
			// Encrypted for another Windows account or computer (settings.json
			// was copied): it cannot be read here, so it is not used.
			fmt.Println("The saved Cloudflare API token was encrypted for another Windows account or computer; paste it again in Settings.")
			plain = ""
		}
		s.RadarToken = plain
	} else if s.RadarToken != "" && secretSupported() {
		migrate = true // saved as text by an older version
	} else if s.RadarToken != "" {
		_ = os.Chmod(path, 0o600) // an older version wrote it readable by everyone
	}
	s.sanitize()
	if migrate {
		_ = saveSettings(s)
	}
	return s
}

// saveSettings writes the token encrypted where Windows can encrypt it, and
// the file readable by this user only elsewhere.
func saveSettings(s Settings) error {
	if s.RadarToken != "" {
		enc, err := protectSecret(s.RadarToken)
		if err != nil {
			return err
		}
		s.RadarToken = enc
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return writeFileMode(filepath.Join(baseDir, "settings.json"), b, 0o600)
}

// ------------------------------------------------------------------ History

const historyKeep = 100

func loadHistory() []*Result {
	historyMu.Lock()
	defer historyMu.Unlock()
	return readHistory()
}

func readHistory() []*Result {
	list := []*Result{}
	path := filepath.Join(baseDir, "history.json")
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &list); err != nil {
			// Never write over a damaged history: put it aside so nothing is lost.
			keepDamaged(path)
			return []*Result{}
		}
	}
	return list
}

func saveHistory(list []*Result) error {
	if len(list) > historyKeep {
		list = list[len(list)-historyKeep:]
	}
	b, _ := json.Marshal(list)
	return writeFileAtomic(filepath.Join(baseDir, "history.json"), b)
}

func appendHistory(r *Result) error {
	historyMu.Lock()
	defer historyMu.Unlock()
	list := readHistory()
	list = append(list, r)
	return saveHistory(list)
}

// keepDamaged renames a file that could not be read, so a fresh one can be
// started without destroying the old one.
func keepDamaged(path string) {
	bak := path + ".damaged-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, bak); err == nil {
		fmt.Println("Could not read " + filepath.Base(path) + "; kept it as " + filepath.Base(bak))
	}
}

func writeFileAtomic(path string, b []byte) error { return writeFileMode(path, b, 0o644) }

func writeFileMode(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp) // WriteFile keeps an old file's mode
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ------------------------------------------------------------------ Live events

// The hub sends test events to every open page (Server-Sent Events). It also
// keeps the events of the latest test, so a page opened or reloaded mid-test
// catches up.
type eventHub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	replay [][]byte
	hook   func(map[string]any) // command-line mode: progress to the console
}

func newHub() *eventHub { return &eventHub{subs: map[chan []byte]struct{}{}} }

func (h *eventHub) publish(ev map[string]any) {
	if h.hook != nil {
		h.hook(ev)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ev["type"] == "reset" {
		h.replay = h.replay[:0]
	}
	h.replay = append(h.replay, b)
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // a page that cannot keep up misses an event rather than slowing the test
		}
	}
}

func (h *eventHub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 4096)
	h.mu.Lock()
	backlog := append([][]byte(nil), h.replay...)
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()

	send := func(b []byte) error {
		_, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
		return err
	}
	if _, err := w.Write([]byte("event: hello\ndata: {}\n\n")); err != nil {
		return
	}
	for _, b := range backlog {
		if send(b) != nil {
			return
		}
	}
	fl.Flush()

	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			if send(b) != nil {
				return // the page has gone
			}
			// send whatever else is queued in the same write
		drain:
			for i := 0; i < 64; i++ {
				select {
				case b2 := <-ch:
					if send(b2) != nil {
						return
					}
				default:
					break drain
				}
			}
			fl.Flush()
		case <-keep.C:
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
