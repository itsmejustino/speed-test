package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type interrupt struct {
	hit atomic.Bool
	ch  chan os.Signal
}

func (i *interrupt) stop() { signal.Stop(i.ch) }

// watchInterrupt cancels on Ctrl+C and remembers that it did.
func watchInterrupt(cancel func()) *interrupt {
	i := &interrupt{ch: make(chan os.Signal, 1)}
	signal.Notify(i.ch, os.Interrupt)
	go func() {
		if _, ok := <-i.ch; ok {
			i.hit.Store(true)
			cancel()
		}
	}()
	return i
}

// Comparison mode: runs this test and other well-known tests back to back,
// several rounds, rotating the order so time of day and traffic affect each
// one equally, then reports the median and spread of each.
//
//	SpeedTest.exe -compare -rounds 10
//
// Other tests are used when their command-line programs are found next to
// SpeedTest.exe, in the data folder, or on the PATH:
//   - M-Lab NDT7: ndt7-client (github.com/m-lab/ndt7-client-go). One TCP
//     connection to an M-Lab server outside your ISP's network.
//   - Ookla Speedtest: speedtest (the official Speedtest CLI). Several
//     connections, usually to a server inside your ISP's network. Its licence
//     must be accepted: run it once yourself, or pass -accept-ookla-license.

type sample struct {
	Tool     string  `json:"tool"`
	Round    int     `json:"round"`
	When     string  `json:"when"`
	Down     float64 `json:"downloadMbps"`
	Up       float64 `json:"uploadMbps"`
	Ping     float64 `json:"idlePingMs"`
	Jitter   float64 `json:"jitterMs,omitempty"`
	LoadedDn float64 `json:"loadedDownMs,omitempty"`
	LoadedUp float64 `json:"loadedUpMs,omitempty"`
	Loss     float64 `json:"lossPct"`
	Server   string  `json:"server"`
	Error    string  `json:"error,omitempty"`
}

type tool struct {
	name, path string
	run        func(ctx context.Context) (sample, error)
}

func findProgram(names ...string) string {
	dirs := []string{baseDir}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, n := range names {
		if runtime.GOOS == "windows" && !strings.HasSuffix(n, ".exe") {
			n += ".exe"
		}
		for _, d := range dirs {
			p := filepath.Join(d, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func runProgram(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = nil // a program asking a question gets no answer instead of hanging
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return out.Bytes(), fmt.Errorf("%v: %s", err, msg)
	}
	return out.Bytes(), nil
}

// parseNDT7 reads ndt7-client -format json: progress lines, then a summary
// object with Download, Upload and MinRTT, each {"Value":…, "Unit":…}.
func parseNDT7(out []byte) (sample, error) {
	var s sample
	type val struct {
		Value float64
		Unit  string
	}
	var found bool
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m struct {
			ServerFQDN      string
			Download        *val
			Upload          *val
			MinRTT          *val
			DownloadRetrans *val
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Download == nil || m.Upload == nil {
			continue
		}
		found = true
		s.Down, s.Up = toMbps(m.Download.Value, m.Download.Unit), toMbps(m.Upload.Value, m.Upload.Unit)
		if m.MinRTT != nil {
			s.Ping = m.MinRTT.Value
		}
		if m.DownloadRetrans != nil {
			s.Loss = m.DownloadRetrans.Value
		}
		s.Server = m.ServerFQDN
	}
	if !found {
		return s, errors.New("no summary in ndt7-client output")
	}
	return s, nil
}

func toMbps(v float64, unit string) float64 {
	switch strings.ToLower(unit) {
	case "kbit/s", "kb/s":
		return v / 1000
	case "gbit/s", "gb/s":
		return v * 1000
	}
	return v
}

// parseOokla reads speedtest --format=json: bandwidth in bytes per second.
func parseOokla(out []byte) (sample, error) {
	var s sample
	var m struct {
		Type             string
		Ping             struct{ Latency, Jitter float64 }
		Download, Upload struct {
			Bandwidth float64
			Latency   struct{ Iqm float64 }
		}
		PacketLoss *float64
		Server     struct{ Name, Location string }
	}
	// The last JSON object on the output is the result.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if json.Unmarshal([]byte(lines[i]), &m) == nil && m.Type == "result" {
			s.Down, s.Up = m.Download.Bandwidth*8/1e6, m.Upload.Bandwidth*8/1e6
			s.Ping, s.Jitter = m.Ping.Latency, m.Ping.Jitter
			s.LoadedDn, s.LoadedUp = m.Download.Latency.Iqm, m.Upload.Latency.Iqm
			if m.PacketLoss != nil {
				s.Loss = *m.PacketLoss
			}
			s.Server = strings.TrimSpace(m.Server.Name + " " + m.Server.Location)
			return s, nil
		}
	}
	return s, errors.New("no result in speedtest output")
}

func compareTools(acceptOokla bool) []tool {
	tools := []tool{{name: "Speed Test", run: func(ctx context.Context) (sample, error) {
		settingsMu.Lock()
		cfg := settings
		settingsMu.Unlock()
		r := newRun(cfg)
		stop := context.AfterFunc(ctx, r.cancel)
		defer stop()
		res, err := r.run()
		r.cancel()
		if err != nil {
			return sample{}, err
		}
		_ = appendHistory(res)
		return sample{
			Down: res.Download.Mbps, Up: res.Upload.Mbps, Ping: res.Idle.Median, Jitter: res.Idle.Jitter,
			LoadedDn: res.Download.Latency.Median, LoadedUp: res.Upload.Latency.Median, Loss: res.LossPct,
			Server: "Cloudflare " + res.Server.Colo,
		}, nil
	}}}
	if p := findProgram("ndt7-client"); p != "" {
		tools = append(tools, tool{name: "M-Lab NDT7", path: p, run: func(ctx context.Context) (sample, error) {
			out, err := runProgram(ctx, p, "-format", "json", "-quiet")
			if err != nil {
				return sample{}, err
			}
			return parseNDT7(out)
		}})
	}
	if p := findProgram("speedtest"); p != "" {
		tools = append(tools, tool{name: "Ookla Speedtest", path: p, run: func(ctx context.Context) (sample, error) {
			args := []string{"--format=json", "--progress=no"}
			if acceptOokla {
				args = append(args, "--accept-license", "--accept-gdpr")
			}
			out, err := runProgram(ctx, p, args...)
			if err != nil {
				if !acceptOokla && strings.Contains(strings.ToLower(err.Error()), "license") {
					err = errors.New("its licence has not been accepted: run speedtest once yourself, or add -accept-ookla-license")
				}
				return sample{}, err
			}
			return parseOokla(out)
		}})
	}
	return tools
}

func runCompare(rounds int, pause time.Duration, acceptOokla, asJSON bool, stdout, stderr io.Writer) int {
	tools := compareTools(acceptOokla)
	logf := func(f string, a ...any) { _, _ = fmt.Fprintf(stderr, f+"\n", a...) }
	for _, t := range tools[1:] {
		logf("Found %s: %s", t.name, t.path)
	}
	if len(tools) == 1 {
		logf("No other tests found. Put ndt7-client and/or speedtest (Ookla's CLI) next to SpeedTest.exe or on the PATH to compare; running this test alone for its spread.")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := watchInterrupt(cancel)
	defer interrupted.stop()
	var all []sample
	for round := 1; round <= rounds && ctx.Err() == nil; round++ {
		for i := range tools {
			t := tools[(i+round-1)%len(tools)] // rotate the order each round
			if ctx.Err() != nil {
				break
			}
			logf("Round %d of %d: %s...", round, rounds, t.name)
			tctx, tcancel := context.WithTimeout(ctx, 3*time.Minute)
			s, err := t.run(tctx)
			tcancel()
			s.Tool, s.Round, s.When = t.name, round, time.Now().Format(time.RFC3339)
			if err != nil {
				s.Error = err.Error()
				logf("  %s failed: %s", t.name, s.Error)
			} else {
				logf("  %.1f down, %.1f up, %.1f ms", s.Down, s.Up, s.Ping)
			}
			all = append(all, s)
			if ctx.Err() == nil {
				select {
				case <-time.After(pause):
				case <-ctx.Done():
				}
			}
		}
	}
	if interrupted.hit.Load() {
		logf("Stopped; summarising what was measured.")
	}

	name := filepath.Join(baseDir, "compare-"+time.Now().Format("20060102-150405")+".csv")
	if err := writeCompareCSV(name, all); err != nil {
		logf("Could not write %s: %v", name, err)
	} else {
		logf("Every run is in %s", name)
	}
	sum := summarise(tools, all)
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"summary": sum, "runs": all})
	} else {
		_, _ = io.WriteString(stdout, compareReport(sum, rounds))
	}
	ok := false
	for _, s := range all {
		if s.Error == "" && s.Tool == "Speed Test" {
			ok = true
		}
	}
	if !ok {
		return 1
	}
	return 0
}

type stat struct {
	N                   int
	Median, P25, P75    float64
	Min, Max, SpreadPct float64 // spread: interquartile range as a % of the median
}

type toolSummary struct {
	Tool             string `json:"tool"`
	Runs, Failed     int
	Down, Up, Ping   stat
	LoadedDn         stat `json:"loadedDown"`
	LoadedUp         stat `json:"loadedUp"`
	Servers          []string
	FirstError       string `json:"firstError,omitempty"`
	MeasuresAgainst  string `json:"measures"`
	ConnectionsUsed  string `json:"connections"`
	DownVsOursPct    float64
	UpVsOursPct      float64
	HasLoadedLatency bool
}

func statOf(v []float64) stat {
	var s stat
	var x []float64
	for _, f := range v {
		if f > 0 && !math.IsNaN(f) {
			x = append(x, f)
		}
	}
	s.N = len(x)
	if s.N == 0 {
		return s
	}
	sort.Float64s(x)
	s.Median, s.P25, s.P75 = percentile(x, 50), percentile(x, 25), percentile(x, 75)
	s.Min, s.Max = x[0], x[len(x)-1]
	if s.Median > 0 {
		s.SpreadPct = (s.P75 - s.P25) / s.Median * 100
	}
	return s
}

func summarise(tools []tool, all []sample) []toolSummary {
	var out []toolSummary
	for _, t := range tools {
		ts := toolSummary{Tool: t.name}
		var dn, up, pg, ld, lu []float64
		seen := map[string]bool{}
		for _, s := range all {
			if s.Tool != t.name {
				continue
			}
			ts.Runs++
			if s.Error != "" {
				ts.Failed++
				if ts.FirstError == "" {
					ts.FirstError = s.Error
				}
				continue
			}
			dn, up, pg, ld, lu = append(dn, s.Down), append(up, s.Up), append(pg, s.Ping), append(ld, s.LoadedDn), append(lu, s.LoadedUp)
			if s.Server != "" && !seen[s.Server] {
				seen[s.Server] = true
				ts.Servers = append(ts.Servers, s.Server)
			}
		}
		ts.Down, ts.Up, ts.Ping, ts.LoadedDn, ts.LoadedUp = statOf(dn), statOf(up), statOf(pg), statOf(ld), statOf(lu)
		ts.HasLoadedLatency = ts.LoadedDn.N > 0
		switch t.name {
		case "Speed Test":
			ts.MeasuresAgainst, ts.ConnectionsUsed = "Cloudflare's nearest data centre", "several connections"
		case "M-Lab NDT7":
			ts.MeasuresAgainst, ts.ConnectionsUsed = "an M-Lab server outside your ISP's network", "one connection"
		case "Ookla Speedtest":
			ts.MeasuresAgainst, ts.ConnectionsUsed = "a Speedtest server, often inside your ISP's network", "several connections"
		}
		out = append(out, ts)
	}
	if len(out) > 0 && out[0].Down.N > 0 {
		for i := 1; i < len(out); i++ {
			if out[i].Down.N > 0 {
				out[i].DownVsOursPct = (out[i].Down.Median/out[0].Down.Median - 1) * 100
				out[i].UpVsOursPct = (out[i].Up.Median/out[0].Up.Median - 1) * 100
			}
		}
	}
	return out
}

func compareReport(sum []toolSummary, rounds int) string {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	p("")
	p("Comparison, %d round(s), %s", rounds, time.Now().Format("2006-01-02 15:04 MST"))
	p("Median (spread = middle half of the runs, as %% of the median; lower is steadier)")
	p("")
	p("%-16s %5s  %-20s %-20s %-16s %-13s", "Test", "Runs", "Download Mbps", "Upload Mbps", "Idle ping ms", "Loaded ms")
	for _, t := range sum {
		ok := t.Runs - t.Failed
		if ok == 0 {
			p("%-16s %5s  all runs failed: %s", t.Tool, fmt.Sprintf("0/%d", t.Runs), t.FirstError)
			continue
		}
		loaded := "-"
		if t.HasLoadedLatency {
			loaded = fmt.Sprintf("%.1f / %.1f", t.LoadedDn.Median, t.LoadedUp.Median)
		}
		p("%-16s %5s  %-20s %-20s %-16s %-13s", t.Tool, fmt.Sprintf("%d/%d", ok, t.Runs),
			fmt.Sprintf("%.1f ±%.0f%%", t.Down.Median, t.Down.SpreadPct),
			fmt.Sprintf("%.1f ±%.0f%%", t.Up.Median, t.Up.SpreadPct),
			fmt.Sprintf("%.1f ±%.0f%%", t.Ping.Median, t.Ping.SpreadPct), loaded)
	}
	p("")
	for _, t := range sum[1:] {
		if t.Down.N == 0 {
			continue
		}
		p("%s vs Speed Test: download %+.1f%%, upload %+.1f%%.", t.Tool, t.DownVsOursPct, t.UpVsOursPct)
	}
	p("")
	for _, t := range sum {
		if t.MeasuresAgainst == "" {
			continue
		}
		srv := ""
		if len(t.Servers) > 0 {
			srv = " (" + strings.Join(t.Servers, "; ") + ")"
		}
		p("%s: %s to %s%s.", t.Tool, t.ConnectionsUsed, t.MeasuresAgainst, srv)
	}
	p("Differences between tests are expected: they use different servers, routes and numbers of connections. Spread shows how repeatable each one is on this line.")
	return b.String()
}

func writeCompareCSV(name string, all []sample) error {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"round", "tool", "when", "download_mbps", "upload_mbps", "idle_ping_ms", "jitter_ms", "loaded_down_ms", "loaded_up_ms", "loss_pct", "server", "error"})
	n := func(v float64) string {
		if v == 0 {
			return ""
		}
		return strconv.FormatFloat(v, 'f', 3, 64)
	}
	for _, s := range all {
		_ = w.Write([]string{strconv.Itoa(s.Round), s.Tool, s.When, n(s.Down), n(s.Up), n(s.Ping), n(s.Jitter), n(s.LoadedDn), n(s.LoadedUp), n(s.Loss), s.Server, s.Error})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
