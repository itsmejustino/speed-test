// SpeedTest - a native speed test engine with a web page for its display.
//
// The program measures from this computer (not from the browser): separate TCP
// connections for each download and upload stream, a steady ping stream for
// latency, jitter and packet loss, and Cloudflare's own metadata for where you
// and the server are. The page at http://localhost:8765/ shows the results.
//
// Settings and history are kept next to the program in settings.json and
// history.json.
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

var (
	baseDir string
	webDir  string

	settingsMu sync.Mutex
	settings   Settings

	historyMu sync.Mutex

	runMu   sync.Mutex
	current *Run

	hub = newHub()
)

func main() {
	port := flag.Int("port", 8765, "port for the local page")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser")
	flag.StringVar(&webDir, "web", "", "serve index.html from this folder instead of the built-in copy")
	flag.StringVar(&baseDir, "data", "", "folder for settings.json and history.json (default: next to the program)")
	flag.Parse()

	if baseDir == "" {
		exe, err := os.Executable()
		if err == nil {
			baseDir = filepath.Dir(exe)
		} else {
			baseDir = "."
		}
	}
	settings = loadSettings()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/settings", handleSettings)
	mux.HandleFunc("/api/history", handleHistory)
	mux.HandleFunc("/api/checktoken", handleCheckToken)
	mux.HandleFunc("/api/start", handleStart)
	mux.HandleFunc("/api/stop", handleStop)
	mux.HandleFunc("/api/state", handleState)
	mux.HandleFunc("/api/events", hub.serveSSE)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("Could not start on %s: %v\n", addr, err)
		fmt.Println("If another copy is already running, close it, or start this one with -port 8766")
		fmt.Print("Press Enter to exit")
		_, _ = fmt.Scanln()
		os.Exit(1)
	}
	url := fmt.Sprintf("http://localhost:%d/", *port)
	fmt.Println("Speed test is running at " + url)
	fmt.Println("Leave this window open while you use it. Close it (or press Ctrl+C) to stop.")
	if !*noBrowser {
		openBrowser(url)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fmt.Println(err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("Could not open the browser; open " + url + " yourself.")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // the page may have gone; nothing to do then
}

// Requests that change something must carry this header. A web page on another
// site cannot add it without the browser asking this server first, so other
// sites cannot start tests or change settings behind your back.
func guarded(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || r.Header.Get("X-Speedtest") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return false
	}
	return true
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	body := indexHTML
	if webDir != "" {
		if b, err := os.ReadFile(filepath.Join(webDir, "index.html")); err == nil {
			body = b
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		settingsMu.Lock()
		s := settings
		settingsMu.Unlock()
		writeJSON(w, 200, s.public())
		return
	}
	if !guarded(w, r) {
		return
	}
	var in Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad settings"})
		return
	}
	settingsMu.Lock()
	if in.RadarToken == keepToken {
		in.RadarToken = settings.RadarToken
	}
	in.sanitize()
	settings = in
	err := saveSettings(settings)
	s := settings
	settingsMu.Unlock()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, s.public())
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, loadHistory())
		return
	}
	if !guarded(w, r) {
		return
	}
	// POST {"clear":true} empties the history.
	var in struct {
		Clear bool `json:"clear"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) // an empty body means "just list"
	if in.Clear {
		historyMu.Lock()
		err := saveHistory([]*Result{})
		historyMu.Unlock()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not clear history: " + err.Error()})
			return
		}
	}
	writeJSON(w, 200, loadHistory())
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if !guarded(w, r) {
		return
	}
	runMu.Lock()
	defer runMu.Unlock()
	if current != nil {
		writeJSON(w, 409, map[string]string{"error": "a test is already running"})
		return
	}
	settingsMu.Lock()
	s := settings
	settingsMu.Unlock()
	run := newRun(s)
	current = run
	go func() {
		run.execute()
		runMu.Lock()
		current = nil
		runMu.Unlock()
	}()
	writeJSON(w, 200, map[string]bool{"started": true})
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if !guarded(w, r) {
		return
	}
	runMu.Lock()
	if current != nil {
		current.cancel()
	}
	runMu.Unlock()
	writeJSON(w, 200, map[string]bool{"stopping": true})
}

func handleState(w http.ResponseWriter, r *http.Request) {
	runMu.Lock()
	running := current != nil
	runMu.Unlock()
	writeJSON(w, 200, map[string]any{"running": running, "os": runtime.GOOS})
}

// Checks a token against Radar: the one typed in Settings, or the saved one.
func handleCheckToken(w http.ResponseWriter, r *http.Request) {
	if !guarded(w, r) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in) // empty: check the saved token
	tok := cleanToken(in.Token)
	if tok == "" || tok == keepToken {
		settingsMu.Lock()
		tok = settings.RadarToken
		settingsMu.Unlock()
	}
	if tok == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "message": "No token to check. Paste one first."})
		return
	}
	if err := checkRadarToken(r.Context(), tok); err != nil {
		msg := err.Error()
		writeJSON(w, 200, map[string]any{"ok": false, "message": strings.ToUpper(msg[:1]) + msg[1:] + "."})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "The token works: Radar answered."})
}
