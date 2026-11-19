package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"
)

// Command-line mode, for scripts and remote-management tools:
//
//	SpeedTest.exe -run            a summary for people
//	SpeedTest.exe -run -json      the whole result as JSON
//
// Progress goes to stderr, the result to stdout. The exit code is 0 when the
// test finished and 1 when it failed or was stopped (Ctrl+C).
func runCLI(asJSON bool, seconds float64, stdout, stderr io.Writer) int {
	settingsMu.Lock()
	cfg := settings
	settingsMu.Unlock()
	if seconds > 0 {
		cfg.DirectionSeconds = seconds
		cfg.sanitize()
	}
	run := newRun(cfg)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)
	go func() {
		select {
		case <-stop:
			run.cancel()
		case <-run.ctx.Done():
		}
	}()

	last := ""
	hub.hook = func(ev map[string]any) {
		if ev["type"] == "status" {
			if t, _ := ev["text"].(string); t != last {
				last = t
				_, _ = fmt.Fprintln(stderr, t)
			}
		}
	}
	defer func() { hub.hook = nil }()

	res, err := run.run()
	interrupted := run.ctx.Err() != nil
	run.cancel()
	if err != nil {
		if interrupted {
			_, _ = fmt.Fprintln(stderr, "Stopped.")
		} else {
			_, _ = fmt.Fprintln(stderr, "Error: "+err.Error())
		}
		return 1
	}
	if err := appendHistory(res); err != nil {
		res.Warnings = append(res.Warnings, "Could not save to history.json: "+err.Error())
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		_, _ = io.WriteString(stdout, textReport(res))
	}
	return 0
}

// textReport is a plain-text summary of one test, for a ticket or a chat.
func textReport(r *Result) string {
	var b strings.Builder
	line := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	f1 := func(v float64) string { return fmt.Sprintf("%.1f", v) }

	line("Speed Test report, %s", time.UnixMilli(r.When).Format("2006-01-02 15:04 MST"))
	c, s := r.Client, r.Server
	who := []string{}
	for _, x := range []string{c.IP, ispName(c), place(c.City, c.Region, c.Country)} {
		if x != "" {
			who = append(who, x)
		}
	}
	line("From:        %s", strings.Join(who, " · "))
	srv := "Cloudflare " + s.Colo
	if s.City != "" {
		srv += " (" + s.City + ")"
	}
	if s.IP != "" {
		srv += " · " + s.IP
	}
	if r.DistanceKm > 0 {
		srv += fmt.Sprintf(" · %.0f km away, physics floor %.2f ms", r.DistanceKm, r.FiberFloorMs)
	}
	line("Server:      %s", srv)
	if a := r.Adapter; a != nil {
		parts := []string{a.Kind + " \"" + a.Name + "\""}
		if a.LinkDownMbps > 0 {
			parts = append(parts, fmt.Sprintf("link %s/%s Mbps", trimNum(a.LinkDownMbps), trimNum(a.LinkUpMbps)))
		}
		parts = append(parts, "local "+a.LocalIP)
		if len(a.Gateways) > 0 {
			parts = append(parts, "gateway "+strings.Join(a.Gateways, ", "))
		}
		if len(a.DNS) > 0 {
			parts = append(parts, "DNS "+strings.Join(a.DNS, ", "))
		}
		if a.MTU > 0 {
			parts = append(parts, fmt.Sprintf("MTU %d", a.MTU))
		}
		line("Adapter:     %s", strings.Join(parts, " · "))
	}
	if st := r.Setup; st != nil {
		v := ""
		if st.TLSVersion != "" {
			v = ", " + st.TLSVersion
		}
		tlsPart := ""
		if st.TLSMs > 0 {
			tlsPart = " · TLS " + f1(st.TLSMs) + " ms"
		}
		line("Setup:       DNS %s ms · TCP %s ms%s · first byte %s ms (%s%s)", f1(st.DNSMs), f1(st.TCPMs), tlsPart, f1(st.TTFBMs), st.Protocol, v)
	}
	line("")
	phase := func(name string, p PhaseStats) {
		line("%-12s %8s Mbps   median %s · 90th %s   loaded ping %s ms (+%s)", name, f1(p.Mbps), f1(p.Median), f1(p.P90), f1(p.Latency.Median), f1(math.Max(0, p.BloatMs)))
	}
	phase("Download", r.Download)
	phase("Upload", r.Upload)
	line("%-12s %8s ms     jitter %s ms · loss %s%% of %d pings (%s)", "Idle ping", f1(r.Idle.Median), f1(r.Idle.Jitter), f1(r.Idle.LossPct), r.Idle.Sent, r.ProbeMethod)
	bloat := math.Max(0, math.Max(r.Download.BloatMs, r.Upload.BloatMs))
	line("%-12s %s (+%s ms under load, Cloudflare AIM bands)", "Bufferbloat", gradeFor(bloat), f1(bloat))
	line("%-12s %s%% of %d pings over the whole test", "Loss", f1(r.LossPct), r.ProbesSent)
	if d, u := r.Download.Server, r.Upload.Server; d != nil {
		t := fmt.Sprintf("min RTT %s ms · resent %.2f%% downloading", f1(d.MinRTT), d.RetransPct)
		if u != nil {
			t += fmt.Sprintf(", %.2f%% uploading", u.RetransPct)
		}
		line("%-12s %s", "Server TCP", t)
	}
	if r.TCP != nil {
		line("%-12s %.2f%% of %d segments resent", "Local TCP", r.TCP.RatePct, r.TCP.SegmentsSent)
	}
	if len(r.Route) > 0 {
		line("")
		line("Route:")
		for _, h := range r.Route {
			who := h.IP
			if who == "" {
				who = "*"
			}
			if h.Host != "" {
				who = h.Host + " (" + h.IP + ")"
			}
			pings := []string{}
			for _, v := range h.RTTs {
				if v < 0 {
					pings = append(pings, "*")
				} else {
					pings = append(pings, f1(v))
				}
			}
			line("  %2d  %-48s %s", h.TTL, who, strings.Join(pings, "  "))
		}
	}
	for _, w := range r.Warnings {
		line("Note: %s", w)
	}
	line("")
	line("Test: %s s each way, %d connections, %s s warm-up left out", trimNum(r.Config.DirectionSeconds), r.Config.Streams, trimNum(r.Config.WarmupSeconds))
	return b.String()
}

func ispName(c ClientInfo) string {
	switch {
	case c.ISP != "" && c.ASN != 0:
		return fmt.Sprintf("%s (AS%d)", c.ISP, c.ASN)
	case c.ASN != 0:
		return fmt.Sprintf("AS%d", c.ASN)
	}
	return c.ISP
}

func place(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

func trimNum(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// The same bands as the page: Cloudflare AIM's loaded-minus-idle latency.
func gradeFor(ms float64) string {
	switch {
	case ms < 10:
		return "A"
	case ms < 20:
		return "B"
	case ms < 50:
		return "C"
	}
	return "D"
}
