package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// A stand-in for speed.cloudflare.com: /meta only for "its own page", a colo
// record, downloads and uploads shaped to modest speeds, and a cfL4 entry on
// every response.
func fakeCloudflare(t *testing.T) *httptest.Server {
	var sent, retrans int64
	timing := func(w http.ResponseWriter) {
		s := atomic.AddInt64(&sent, 400)
		rt := atomic.AddInt64(&retrans, 2)
		w.Header().Set("Server-Timing", fmt.Sprintf(`cfL4;desc="?proto=TCP&rtt=15551&min_rtt=12096&rtt_var=5109&sent=%d&recv=17&lost=0&retrans=%d&x=0"`, s, rt))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") == "" {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, "{}")
			return
		}
		_, _ = io.WriteString(w, `{"clientIp":"73.14.122.9","asn":7922,"asOrganization":"Comcast Cable Communications Holdings, Inc","country":"US","city":"Aurora","region":"Colorado","latitude":"39.72943","longitude":"-104.83192","colo":{"iata":"DEN","lat":39.861698,"lon":-104.672997,"cca2":"US","city":"Denver"}}`)
	})
	buf := make([]byte, 32<<10)
	mux.HandleFunc("/__down", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		timing(w)
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		for n > 0 {
			k := int64(len(buf))
			if n < k {
				k = n
			}
			if _, err := w.Write(buf[:k]); err != nil {
				return
			}
			n -= k
			time.Sleep(200 * time.Microsecond)
		}
	})
	mux.HandleFunc("/__up", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 32<<10)
		for {
			_, err := r.Body.Read(b)
			if err != nil {
				break
			}
			time.Sleep(400 * time.Microsecond)
		}
		timing(w)
		_, _ = io.WriteString(w, "ok")
	})
	return httptest.NewServer(mux)
}

func TestFullRun(t *testing.T) {
	srv := fakeCloudflare(t)
	defer srv.Close()
	baseDir = t.TempDir()

	// Stand in for the network on the way: four routers, then the server.
	routeSupported = func() bool { return true }
	probeTTL = func(ip net.IP, ttl int, _ time.Duration) (net.IP, time.Duration, bool, error) {
		if ttl >= 5 {
			return ip, time.Duration(10+ttl) * time.Millisecond, true, nil
		}
		if ttl == 3 {
			return nil, 0, false, fmt.Errorf("no reply")
		}
		return net.IPv4(10, 0, 0, byte(ttl)), time.Duration(ttl) * time.Millisecond, false, nil
	}
	defer func() { routeSupported = icmpSupported; probeTTL = icmpProbeTTL }()

	cfg := defaultSettings()
	cfg.Server = srv.URL
	cfg.DirectionSeconds, cfg.IdleSeconds, cfg.WarmupSeconds = 3, 1, 0.75
	cfg.Streams, cfg.ProbeRate = 3, 10
	run := newRun(cfg)
	res, err := run.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Client.ISP == "" || !res.Client.HasGeo || res.Server.Colo != "DEN" || !res.Server.HasGeo {
		t.Errorf("meta not read: %+v %+v", res.Client, res.Server)
	}
	if res.DistanceKm < 10 || res.DistanceKm > 30 {
		t.Errorf("distance %.1f km", res.DistanceKm)
	}
	if res.Download.Mbps <= 0 || res.Upload.Mbps <= 0 {
		t.Errorf("speeds %.1f / %.1f", res.Download.Mbps, res.Upload.Mbps)
	}
	if res.Download.Server == nil || res.Download.Server.Retrans == 0 || res.Download.Server.MinRTT != 12.096 {
		t.Errorf("server TCP view not read: %+v", res.Download.Server)
	}
	if len(res.Route) != 5 || !res.Route[4].Dest || res.Route[2].IP != "" || res.Route[0].IP != "10.0.0.1" {
		t.Errorf("route: %+v", res.Route)
	}
	if res.ProbesSent == 0 || res.Idle.Sent == 0 {
		t.Errorf("no pings recorded")
	}
	t.Logf("down %.1f Mbps, up %.1f Mbps, idle %.2f ms, server rtt %.1f ms, retrans %.2f%%",
		res.Download.Mbps, res.Upload.Mbps, res.Idle.Median, res.Download.Server.RTT, res.Download.Server.RetransPct)
}

func TestStopMidway(t *testing.T) {
	srv := fakeCloudflare(t)
	defer srv.Close()
	cfg := defaultSettings()
	cfg.Server = srv.URL
	cfg.DirectionSeconds, cfg.IdleSeconds, cfg.WarmupSeconds, cfg.Streams = 5, 1, 0.5, 2
	run := newRun(cfg)
	go func() { time.Sleep(1500 * time.Millisecond); run.cancel() }()
	done := make(chan error, 1)
	go func() { _, err := run.run(); done <- err }()
	select {
	case err := <-done:
		if err == nil || run.ctx.Err() == nil {
			t.Errorf("expected a stop, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop within 10 s")
	}
}

func TestParseCfL4(t *testing.T) {
	s, ok := parseCfL4(`cfSpeedEdge;dur=2, cfL4;desc="?proto=TCP&rtt=15551&min_rtt=12096&rtt_var=5109&sent=23&recv=17&lost=0&retrans=1&x=0"`)
	if !ok || s.rtt != 15.551 || s.minRTT != 12.096 || s.sent != 23 || s.retrans != 1 {
		t.Errorf("%+v %v", s, ok)
	}
	if _, ok := parseCfL4(`cfSpeedEdge;dur=2`); ok {
		t.Error("parsed nothing as something")
	}
}

func TestLatencyStats(t *testing.T) {
	p := []Probe{{T: 0.1, RTT: 10}, {T: 0.2, RTT: 12}, {T: 0.3, RTT: -1}, {T: 0.4, RTT: 11}, {T: 2, RTT: 50}}
	st := latencyStats(p, 0, 1, 0)
	if st.Sent != 4 || st.Lost != 1 || st.Median != 11 || st.Min != 10 || st.Jitter != 1.5 {
		t.Errorf("%+v", st)
	}
	_ = context.Background
}

func TestDamagedHistoryIsKept(t *testing.T) {
	baseDir = t.TempDir()
	path := baseDir + "/history.json"
	if err := os.WriteFile(path, []byte(`[{"id":"a"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendHistory(&Result{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(path + ".damaged-*")
	if len(matches) != 1 {
		t.Fatalf("damaged file not kept: %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != `[{"id":"a"` {
		t.Errorf("damaged file changed: %q", b)
	}
	if list := loadHistory(); len(list) != 1 || list[0].ID != "b" {
		t.Errorf("new history: %+v", list)
	}
}
