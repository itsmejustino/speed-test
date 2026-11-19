package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ------------------------------------------------------------------ Result types

type Sample struct {
	T     float64 `json:"t"`     // seconds since the idle phase began
	Phase string  `json:"phase"` // download | upload
	Mbps  float64 `json:"mbps"`  // over the last 250 ms
	Avg   float64 `json:"avg"`   // over the last second
}

type Probe struct {
	T     float64 `json:"t"`
	Phase string  `json:"phase"`
	RTT   float64 `json:"rtt"` // ms; -1 means no reply (lost)
}

type PhaseMark struct {
	Name  string  `json:"name"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type LatencyStats struct {
	Median  float64 `json:"median"`
	Min     float64 `json:"min"`
	P90     float64 `json:"p90"`
	Jitter  float64 `json:"jitter"`
	LossPct float64 `json:"lossPct"`
	Sent    int     `json:"sent"`
	Lost    int     `json:"lost"`
}

type PhaseStats struct {
	Mbps     float64      `json:"mbps"`   // steady-state average: bytes after the warm-up / time
	Median   float64      `json:"median"` // of the 250 ms readings after the warm-up
	P90      float64      `json:"p90"`
	Peak     float64      `json:"peak"`
	Bytes    int64        `json:"bytes"`
	Seconds  float64      `json:"seconds"`
	Requests int64        `json:"requests"`
	Streams  int          `json:"streams"`
	FileSize int64        `json:"fileSize"`
	Latency  LatencyStats `json:"latency"` // latency while loaded
	BloatMs  float64      `json:"bloatMs"` // loaded median minus idle median
	Server   *ServerTCP   `json:"server,omitempty"`
	// Upload only: Mbps above is counted from the data Cloudflare confirmed it
	// received (each request's answer); SentMbps is what left this computer,
	// counted as it entered the send buffer.
	Confirmed bool    `json:"confirmed,omitempty"`
	SentMbps  float64 `json:"sentMbps,omitempty"`
}

// What Cloudflare's own TCP stack saw on our connections (its Server-Timing
// "cfL4" entry): round-trip time measured by the server, and how many packets it
// had to send again. On a download, resent packets are packets lost on the way
// to you. Each figure is as of the last response on each connection.
type ServerTCP struct {
	Connections int     `json:"connections"`
	RTT         float64 `json:"rtt"`    // ms, median of the server's smoothed RTT
	MinRTT      float64 `json:"minRtt"` // ms, lowest the server saw
	RTTVar      float64 `json:"rttVar"` // ms, median
	Sent        int64   `json:"sent"`   // packets the server sent
	Retrans     int64   `json:"retrans"`
	RetransPct  float64 `json:"retransPct"`
}

type l4Snapshot struct {
	rtt, minRTT, rttVar float64
	sent, retrans       int64
}

type ServerInfo struct {
	Host    string  `json:"host"`
	IP      string  `json:"ip"`
	Colo    string  `json:"colo"`
	City    string  `json:"city"`
	Country string  `json:"country"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	HasGeo  bool    `json:"hasGeo"`
}

type ClientInfo struct {
	IP      string  `json:"ip"`
	ASN     int     `json:"asn"`
	ISP     string  `json:"isp"`
	City    string  `json:"city"`
	Region  string  `json:"region"`
	Country string  `json:"country"`
	Postal  string  `json:"postal"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	HasGeo  bool    `json:"hasGeo"`
}

type TCPStats struct {
	SegmentsSent  int64   `json:"segmentsSent"`
	Retransmitted int64   `json:"retransmitted"`
	RatePct       float64 `json:"ratePct"`
}

type Result struct {
	ID           string       `json:"id"`
	When         int64        `json:"when"` // ms since 1970
	Config       Settings     `json:"config"`
	Server       ServerInfo   `json:"server"`
	Client       ClientInfo   `json:"client"`
	DistanceKm   float64      `json:"distanceKm"` // 0 when either location is unknown
	FiberFloorMs float64      `json:"fiberFloorMs"`
	ProbeMethod  string       `json:"probeMethod"`
	Idle         LatencyStats `json:"idle"`
	Download     PhaseStats   `json:"download"`
	Upload       PhaseStats   `json:"upload"`
	LossPct      float64      `json:"lossPct"` // all probes, whole test
	ProbesSent   int          `json:"probesSent"`
	ProbesLost   int          `json:"probesLost"`
	TCP          *TCPStats    `json:"tcp,omitempty"`
	Setup        *Setup       `json:"setup,omitempty"`
	Adapter      *Adapter     `json:"adapter,omitempty"`
	Route        []Hop        `json:"route,omitempty"`
	Compare      *Compare     `json:"compare,omitempty"`
	Phases       []PhaseMark  `json:"phases"`
	Samples      []Sample     `json:"samples"`
	Probes       []Probe      `json:"probes"`
	Warnings     []string     `json:"warnings,omitempty"`
}

// ------------------------------------------------------------------ A test run

type Run struct {
	cfg    Settings
	ctx    context.Context
	cancel context.CancelFunc

	host string
	ip   net.IP
	port string
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	t0    time.Time
	phase atomic.Value // string

	mu       sync.Mutex
	samples  []Sample
	probes   []Probe
	phases   []PhaseMark
	warnings []string
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func newRun(cfg Settings) *Run {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Run{cfg: cfg, ctx: ctx, cancel: cancel}
	r.phase.Store("connect")
	return r
}

func (r *Run) t() float64 { return time.Since(r.t0).Seconds() }

func (r *Run) emit(typ string, kv ...any) {
	ev := map[string]any{"type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		ev[kv[i].(string)] = kv[i+1]
	}
	hub.publish(ev)
}

func (r *Run) status(s string) { r.emit("status", "text", s) }

func (r *Run) warn(s string) {
	r.mu.Lock()
	r.warnings = append(r.warnings, s)
	r.mu.Unlock()
}

func (r *Run) execute() {
	defer r.cancel()
	r.emit("reset", "config", r.cfg.public(),
		"plan", map[string]float64{
			"idle": r.cfg.IdleSeconds, "dir": r.cfg.DirectionSeconds, "warmup": r.cfg.WarmupSeconds,
			"total": r.cfg.IdleSeconds + 2*r.cfg.DirectionSeconds,
		})
	res, err := r.run()
	if err != nil {
		if r.ctx.Err() != nil {
			r.emit("error", "kind", "stopped", "message", "Stopped.")
		} else {
			r.emit("error", "kind", "failed", "message", err.Error())
		}
		r.emit("end")
		return
	}
	if err := appendHistory(res); err != nil {
		res.Warnings = append(res.Warnings, "Could not save to history.json: "+err.Error())
	}
	r.emit("result", "result", res)
	r.emit("end")
}

func (r *Run) run() (*Result, error) {
	u, err := url.Parse(r.cfg.Server)
	if err != nil {
		return nil, err
	}
	r.host = u.Hostname()
	r.port = u.Port()
	if r.port == "" {
		if u.Scheme == "http" {
			r.port = "80"
		} else {
			r.port = "443"
		}
	}

	// One address for everything: every stream and every ping goes to the same
	// server, and that is the address reported.
	r.status("Finding the server...")
	dnsStart := time.Now()
	ip, err := resolve(r.ctx, r.host)
	dnsTook := time.Since(dnsStart)
	if err != nil {
		return nil, &testError{"Could not look up " + r.host + ": " + err.Error()}
	}
	r.ip = ip
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	r.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			port = r.port
		}
		return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
	}

	r.status("Reading connection details...")
	adapterCh := make(chan *Adapter, 1)
	go func() { adapterCh <- currentAdapter(ip) }()
	server, client, setup := r.fetchMeta()
	if setup != nil && net.ParseIP(r.host) == nil {
		setup.DNSMs = ms(dnsTook)
	}
	server.Host = r.host
	server.IP = ip.String()
	res := &Result{ID: strconv.FormatInt(time.Now().UnixNano(), 36), When: time.Now().UnixMilli(), Config: r.cfg, Server: server, Client: client}
	res.Config.RadarToken = ""
	res.Setup = setup
	select {
	case res.Adapter = <-adapterCh:
	case <-time.After(2 * time.Second):
	}
	if server.HasGeo && client.HasGeo {
		res.DistanceKm = haversineKm(client.Lat, client.Lon, server.Lat, server.Lon)
		res.FiberFloorMs = fiberFloorMs(res.DistanceKm)
	}

	method, pinger := r.choosePinger()
	res.ProbeMethod = method
	r.emit("meta", "server", server, "client", client, "distanceKm", res.DistanceKm,
		"fiberFloorMs", res.FiberFloorMs, "probeMethod", method, "setup", res.Setup, "adapter", res.Adapter)

	tcpBefore, tcpErr := readTCPCountersQuick()

	// The clock starts with the idle phase.
	r.t0 = time.Now()
	probeCtx, stopProbes := context.WithCancel(r.ctx)
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	go r.probeLoop(probeCtx, &probeWG, pinger, method)

	// Idle: nothing but the pings.
	r.setPhase("idle", "Measuring idle latency...")
	idleStart := r.t()
	if err := sleepCtx(r.ctx, time.Duration(r.cfg.IdleSeconds*float64(time.Second))); err != nil {
		stopProbes()
		return nil, err
	}
	r.markPhase("idle", idleStart, r.t())

	down, err := r.transfer("download")
	if err != nil {
		stopProbes()
		return nil, err
	}
	up, err := r.transfer("upload")
	stopProbes()
	if err != nil {
		return nil, err
	}
	r.status("Finishing...")
	probeWG.Wait()

	// The path to the server, hop by hop.
	if routeSupported() && r.ip.To4() != nil {
		r.status("Tracing the route to the server...")
		rctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		res.Route = traceRoute(rctx, r.ip)
		cancel()
		if r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}
		r.emit("route", "hops", res.Route)
	}

	if tcpErr == nil {
		if after, err := readTCPCountersQuick(); err == nil {
			sent := wrap32(after.sent - tcpBefore.sent)
			re := wrap32(after.retrans - tcpBefore.retrans)
			if sent > 0 && re >= 0 {
				res.TCP = &TCPStats{SegmentsSent: sent, Retransmitted: re, RatePct: float64(re) / float64(sent) * 100}
			}
		}
	}

	r.mu.Lock()
	res.Samples = r.samples
	res.Probes = r.probes
	res.Phases = r.phases
	res.Warnings = append(res.Warnings, r.warnings...)
	r.mu.Unlock()
	sort.Slice(res.Probes, func(i, j int) bool { return res.Probes[i].T < res.Probes[j].T })

	// Latency and loss for each phase, from the pings sent during it. Loaded
	// latency leaves out the warm-up, while the line is still filling.
	for _, ph := range res.Phases {
		switch ph.Name {
		case "idle":
			res.Idle = latencyStats(res.Probes, ph.Start, ph.End, ph.Start)
		case "download":
			down.Latency = latencyStats(res.Probes, ph.Start, ph.End, ph.Start+r.cfg.WarmupSeconds)
		case "upload":
			up.Latency = latencyStats(res.Probes, ph.Start, ph.End, ph.Start+r.cfg.WarmupSeconds)
		}
	}
	if res.Idle.Sent > res.Idle.Lost {
		if down.Latency.Sent > down.Latency.Lost {
			down.BloatMs = down.Latency.Median - res.Idle.Median
		}
		if up.Latency.Sent > up.Latency.Lost {
			up.BloatMs = up.Latency.Median - res.Idle.Median
		}
	}
	res.Download, res.Upload = down, up
	for _, p := range res.Probes {
		res.ProbesSent++
		if p.RTT < 0 {
			res.ProbesLost++
		}
	}
	if res.ProbesSent > 0 {
		res.LossPct = float64(res.ProbesLost) / float64(res.ProbesSent) * 100
	}

	if r.cfg.RadarToken != "" && client.ASN != 0 && client.Country != "" {
		r.status("Comparing with other users...")
		res.Compare = compareWithRadar(r.ctx, r.cfg.RadarToken, client, down.Mbps, up.Mbps, res.Idle.Median)
	}
	return res, nil
}

func (r *Run) setPhase(name, status string) {
	r.phase.Store(name)
	r.emit("phase", "name", name, "t", r.t())
	r.status(status)
}

func (r *Run) markPhase(name string, start, end float64) {
	r.mu.Lock()
	r.phases = append(r.phases, PhaseMark{name, start, end})
	r.mu.Unlock()
	r.emit("phaseEnd", "name", name, "start", start, "end", end)
}

// TCP counters are a nice extra: give them two seconds, then go on without.
func readTCPCountersQuick() (tcpCounters, error) {
	type res struct {
		c   tcpCounters
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := readTCPCounters(); ch <- res{c, err} }()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(2 * time.Second):
		return tcpCounters{}, errors.New("timed out")
	}
}

// Per-family counters are sums of 32-bit values; a difference that comes out
// negative wrapped around once.
func wrap32(d int64) int64 {
	for d < 0 {
		d += 1 << 32
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs { // IPv4 first: Windows' ping API is IPv4
		if a.IP.To4() != nil {
			return a.IP.To4(), nil
		}
	}
	if len(addrs) > 0 {
		return addrs[0].IP, nil
	}
	return nil, errors.New("no address")
}

// ------------------------------------------------------------------ HTTP plumbing

// Each stream gets its own transport, forced to HTTP/1.1, so each stream is its
// own TCP connection. (Over HTTP/2 they would all share one.)
func (r *Run) transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           r.dial,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig:       &tls.Config{ServerName: r.host},
		MaxIdleConnsPerHost:   1,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ReadBufferSize:        256 << 10,
		WriteBufferSize:       256 << 10,
	}
}

var randomBlock = func() []byte {
	b := make([]byte, 1<<20)
	_, _ = rand.Read(b) // never fails on supported systems
	return b
}()

// An upload body made of random bytes, counted as the connection takes them.
type uploadBody struct {
	ctx   context.Context
	left  int64
	off   int
	moved *int64
}

func (u *uploadBody) Read(p []byte) (int, error) {
	if u.ctx.Err() != nil {
		return 0, u.ctx.Err()
	}
	if u.left <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > u.left {
		n = int(u.left)
	}
	if rest := len(randomBlock) - u.off; n > rest {
		n = rest
	}
	copy(p, randomBlock[u.off:u.off+n])
	u.off = (u.off + n) % len(randomBlock)
	u.left -= int64(n)
	atomic.AddInt64(u.moved, int64(n))
	return n, nil
}

// One request. Returns the HTTP status (0 if none came back).
func (r *Run) request(ctx context.Context, c *http.Client, kind string, size int64, moved *int64, onL4 func(l4Snapshot)) (int, error) {
	var req *http.Request
	if kind == "download" {
		req, _ = http.NewRequestWithContext(ctx, "GET", r.cfg.Server+"/__down?bytes="+strconv.FormatInt(size, 10), nil)
	} else {
		body := &uploadBody{ctx: ctx, left: size, moved: moved}
		req, _ = http.NewRequestWithContext(ctx, "POST", r.cfg.Server+"/__up", body)
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	browserHeaders(req, r.cfg.Server)
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if snap, ok := parseCfL4(resp.Header.Get("Server-Timing")); ok && onL4 != nil {
		onL4(snap)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		return resp.StatusCode, nil
	}
	if kind == "upload" {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, nil
	}
	buf := make([]byte, 256<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			atomic.AddInt64(moved, int64(n))
		}
		if err == io.EOF {
			return resp.StatusCode, nil
		}
		if err != nil {
			return resp.StatusCode, err
		}
	}
}

// ------------------------------------------------------------------ A download or upload phase

func (r *Run) transfer(kind string) (PhaseStats, error) {
	label := "Testing download..."
	// 25 MB per request: big enough that turnaround barely shows, small enough
	// that each connection answers often (each answer carries the server's TCP view).
	sizes := []int64{25e6, 10e6}
	if kind == "upload" {
		label = "Testing upload..."
		sizes = []int64{50e6, 25e6, 10e6}
	}
	r.setPhase(kind, label)
	start := r.t()
	began := time.Now()
	dur := time.Duration(r.cfg.DirectionSeconds * float64(time.Second))
	warm := r.cfg.WarmupSeconds

	// About 2 s per download request and 3 s per upload request. Every request
	// costs a pause on its connection while the server answers and the next
	// one starts (Cloudflare's servers take 50 to 300 ms), so requests need to
	// be long enough that the pauses barely show; upload is counted only up to
	// the last answer every connection received, so it can't be cut short.
	target := 2.0
	if kind == "upload" {
		target = 3.0
	}

	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	var moved, requests int64
	var sizeIdx int32
	var failOnce sync.Once
	var failErr error
	fail := func(e error) { failOnce.Do(func() { failErr = e; cancel() }) }

	var wg sync.WaitGroup
	streamsDone := int32(0)
	var l4mu sync.Mutex
	l4 := make([]*l4Snapshot, r.cfg.Streams)
	var doneMu sync.Mutex
	done := make([][]completion, r.cfg.Streams) // uploads the server confirmed, per connection
	for i := 0; i < r.cfg.Streams; i++ {
		wg.Add(1)
		slot := i
		onL4 := func(s l4Snapshot) { l4mu.Lock(); l4[slot] = &s; l4mu.Unlock() }
		go func() {
			defer wg.Done()
			defer atomic.AddInt32(&streamsDone, 1)
			tr := r.transport()
			defer tr.CloseIdleConnections()
			c := &http.Client{Transport: tr}
			errs := 0
			for ctx.Err() == nil {
				idx := atomic.LoadInt32(&sizeIdx)
				size := requestSize(sizes[idx], atomic.LoadInt64(&moved), time.Since(began), r.cfg.Streams, target)
				atomic.AddInt64(&requests, 1)
				status, err := r.request(ctx, c, kind, size, &moved, onL4)
				if ctx.Err() != nil {
					return
				}
				early := time.Since(began) < 3*time.Second
				switch {
				case status == 429:
					fail(&testError{"The Cloudflare speed test service is rate-limiting this connection (HTTP 429). Wait a while, then run again."})
					return
				case status >= 200 && status < 300 && err == nil:
					errs = 0
					if kind == "upload" {
						doneMu.Lock()
						done[slot] = append(done[slot], completion{at: time.Since(began), bytes: size})
						doneMu.Unlock()
					}
					continue
				case early && int(idx) < len(sizes)-1 && (status == 403 || status == 413 || (status == 0 && kind == "upload")):
					// The server turned the file size down: everyone steps down one size.
					atomic.CompareAndSwapInt32(&sizeIdx, idx, idx+1)
					continue
				}
				errs++
				if errs > 4 {
					if atomic.LoadInt64(&moved) > 0 {
						r.warn(fmt.Sprintf("A %s stream kept failing and was dropped (%s).", kind, describe(status, err)))
						return
					}
					fail(&testError{fmt.Sprintf("%s failed: %s.", title(kind), describe(status, err))})
					return
				}
				_ = sleepCtx(ctx, 250*time.Millisecond)
			}
		}()
	}

	// Sampling: every 250 ms, how many bytes have moved.
	type point struct {
		el float64
		b  int64
	}
	pts := []point{{0, 0}}
	var window [4]float64
	n := 0
	tick := time.NewTicker(250 * time.Millisecond)
	lastMove, lastBytes := time.Now(), int64(0)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case now := <-tick.C:
			el := now.Sub(began).Seconds()
			b := atomic.LoadInt64(&moved)
			prev := pts[len(pts)-1]
			pts = append(pts, point{el, b})
			mbps := 0.0
			if el > prev.el {
				mbps = float64(b-prev.b) * 8 / 1e6 / (el - prev.el)
			}
			window[n%4] = mbps
			n++
			cnt := n
			if cnt > 4 {
				cnt = 4
			}
			sum := 0.0
			for i := 0; i < cnt; i++ {
				sum += window[i]
			}
			s := Sample{T: round3(start + el), Phase: kind, Mbps: round3(mbps), Avg: round3(sum / float64(cnt))}
			r.mu.Lock()
			r.samples = append(r.samples, s)
			r.mu.Unlock()
			r.emit("sample", "s", s, "bytes", b)

			if b != lastBytes {
				lastBytes, lastMove = b, now
			} else if now.Sub(lastMove) > 5*time.Second {
				if el > warm+2 && b > 0 {
					r.warn(title(kind) + " stalled before the end; the result uses what was measured until then.")
					cancel()
				} else {
					fail(&testError{title(kind) + " stalled: no data moved for 5 seconds. Check the connection, then run again."})
				}
				break loop
			}
			if el >= dur.Seconds() || atomic.LoadInt32(&streamsDone) == int32(r.cfg.Streams) {
				cancel()
				break loop
			}
		}
	}
	tick.Stop()
	end := r.t()
	waitTimeout(&wg, 5*time.Second)
	r.markPhase(kind, start, end)

	if failErr != nil && r.ctx.Err() == nil {
		return PhaseStats{}, failErr
	}
	if r.ctx.Err() != nil {
		return PhaseStats{}, r.ctx.Err()
	}

	// Steady state: everything after the warm-up.
	first := pts[0]
	for _, p := range pts {
		if p.el >= warm {
			first = p
			break
		}
	}
	last := pts[len(pts)-1]
	if last.el-first.el < 1 {
		first = pts[0]
	}
	if last.el <= first.el || last.b <= first.b {
		return PhaseStats{}, &testError{title(kind) + " moved no data."}
	}
	// Readings over one-second windows, a quarter second apart. Shorter windows
	// mostly measure socket buffers filling and draining, not the line.
	var readings []float64
	for i := 4; i < len(pts); i++ {
		j := i - 4
		if pts[j].el >= first.el && pts[i].el > pts[j].el {
			readings = append(readings, float64(pts[i].b-pts[j].b)*8/1e6/(pts[i].el-pts[j].el))
		}
	}
	st := PhaseStats{
		Mbps:     float64(last.b-first.b) * 8 / 1e6 / (last.el - first.el),
		Bytes:    last.b,
		Seconds:  last.el,
		Requests: atomic.LoadInt64(&requests),
		Streams:  r.cfg.Streams,
		FileSize: sizes[atomic.LoadInt32(&sizeIdx)],
	}
	if len(readings) > 0 {
		st.Median = percentile(readings, 50)
		st.P90 = percentile(readings, 90)
		for _, v := range readings {
			st.Peak = math.Max(st.Peak, v)
		}
	}
	l4mu.Lock()
	st.Server = summarizeL4(l4)
	l4mu.Unlock()
	if kind == "upload" {
		doneMu.Lock()
		c, ok := confirmedMbps(done, time.Duration(warm*float64(time.Second)))
		doneMu.Unlock()
		if ok {
			st.SentMbps, st.Mbps, st.Confirmed = st.Mbps, c, true
		}
	}
	r.emit("phaseResult", "name", kind, "stats", st)
	return st, nil
}

// requestSize picks a request size that takes about the given seconds at the speed
// measured so far: a slow line still completes requests within the test (and
// gets the server's TCP figures, which come with each answer, and for uploads
// its confirmation of what arrived), while a fast one goes straight to the
// largest size. The first request on each connection is 2 MB. Sizes never step
// up in a burst of quick requests: Cloudflare refuses a run of rapid requests
// from one address.
func requestSize(max, moved int64, elapsed time.Duration, streams int, seconds float64) int64 {
	const first = 2e6
	want := int64(first)
	if moved > 0 && elapsed > 250*time.Millisecond && streams > 0 {
		want = int64(float64(moved) / elapsed.Seconds() / float64(streams) * seconds)
	}
	if want < first {
		want = first
	}
	if want > max {
		want = max
	}
	return want
}

// An upload request the server answered: it had received every byte by then.
type completion struct {
	at    time.Duration // since the phase began
	bytes int64
}

// confirmedMbps is upload speed from the server's side. A request's bytes
// all arrived between the previous answer on its connection and its own
// answer, so each request is spread evenly over that interval; the bytes that
// fall inside one window shared by every connection are added up. The window
// runs from the end of the warm-up to the last moment every connection still
// had an answer to come, so nothing cut off at the end is counted. ok is
// false when that window is shorter than a second.
func confirmedMbps(done [][]completion, warm time.Duration) (mbps float64, ok bool) {
	end := time.Duration(-1)
	for _, list := range done {
		if len(list) == 0 {
			continue
		}
		if last := list[len(list)-1].at; end < 0 || last < end {
			end = last
		}
	}
	if end-warm < time.Second {
		return 0, false
	}
	var bytes float64
	for _, list := range done {
		prev := time.Duration(0)
		for _, c := range list {
			lo, hi := max(prev, warm), min(c.at, end)
			if hi > lo && c.at > prev {
				bytes += float64(c.bytes) * float64(hi-lo) / float64(c.at-prev)
			}
			prev = c.at
		}
	}
	return bytes * 8 / 1e6 / (end - warm).Seconds(), true
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

func title(kind string) string {
	if kind == "download" {
		return "Download"
	}
	return "Upload"
}

func describe(status int, err error) string {
	if status != 0 {
		return "HTTP " + strconv.Itoa(status)
	}
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "timed out"
		}
		return "the connection was dropped"
	}
	return "unknown error"
}

// ------------------------------------------------------------------ Pings

type pinger func(ctx context.Context, timeout time.Duration) (time.Duration, error)

// ICMP echo where it works (Windows, IPv4, the server answers); otherwise the
// time to open a TCP connection, which is one round trip.
func (r *Run) choosePinger() (string, pinger) {
	if icmpSupported() && r.ip.To4() != nil {
		ok := int32(0)
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := icmpPing(r.ip, time.Second); err == nil {
					atomic.AddInt32(&ok, 1)
				}
			}()
			time.Sleep(50 * time.Millisecond)
		}
		wg.Wait()
		if ok > 0 {
			ip := r.ip
			return "ICMP echo", func(ctx context.Context, timeout time.Duration) (time.Duration, error) {
				return icmpPing(ip, timeout)
			}
		}
		r.warn("The server did not answer ICMP pings, so latency was measured with TCP connections instead.")
	}
	addr := net.JoinHostPort(r.ip.String(), r.port)
	return "TCP connect", func(ctx context.Context, timeout time.Duration) (time.Duration, error) {
		d := net.Dialer{Timeout: timeout}
		t := time.Now()
		c, err := d.DialContext(ctx, "tcp", addr)
		el := time.Since(t)
		if err != nil {
			return 0, err
		}
		_ = c.Close()
		return el, nil
	}
}

func (r *Run) probeLoop(ctx context.Context, wg *sync.WaitGroup, ping pinger, method string) {
	defer wg.Done()
	rate := r.cfg.ProbeRate
	if method == "TCP connect" && rate > 10 {
		rate = 10 // opening connections is heavier than a ping
	}
	tick := time.NewTicker(time.Second / time.Duration(rate))
	defer tick.Stop()
	var inflight sync.WaitGroup
	defer inflight.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t := r.t()
			phase := r.phase.Load().(string)
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				// A reply has a second to come back. The ping is not tied to ctx: one
				// sent just before the end still gets its full second.
				rtt, err := ping(context.Background(), time.Second)
				if r.ctx.Err() != nil {
					return // stopped by the user: do not count anything
				}
				p := Probe{T: round3(t), Phase: phase, RTT: -1}
				if err == nil {
					p.RTT = round3(float64(rtt.Microseconds()) / 1000)
				}
				r.mu.Lock()
				r.probes = append(r.probes, p)
				r.mu.Unlock()
				r.emit("probe", "p", p)
			}()
		}
	}
}

// ------------------------------------------------------------------ Statistics

func latencyStats(probes []Probe, from, to, steadyFrom float64) LatencyStats {
	var st LatencyStats
	var rtts []float64
	for _, p := range probes {
		if p.T < from || p.T >= to {
			continue
		}
		st.Sent++
		if p.RTT < 0 {
			st.Lost++
			continue
		}
		if p.T >= steadyFrom {
			rtts = append(rtts, p.RTT)
		}
	}
	if st.Sent > 0 {
		st.LossPct = float64(st.Lost) / float64(st.Sent) * 100
	}
	if len(rtts) == 0 {
		return st
	}
	st.Median = percentile(rtts, 50)
	st.P90 = percentile(rtts, 90)
	st.Min = rtts[0]
	sum := 0.0
	for i, v := range rtts {
		st.Min = math.Min(st.Min, v)
		if i > 0 {
			sum += math.Abs(v - rtts[i-1])
		}
	}
	if len(rtts) > 1 {
		st.Jitter = sum / float64(len(rtts)-1) // mean change from one ping to the next
	}
	return st
}

func percentile(v []float64, p float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	pos := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * R * math.Asin(math.Min(1, math.Sqrt(a)))
}

// The fastest a round trip over that distance can be: there and back, at the
// speed of light in glass fibre (about 204,000 km/s, two thirds of c).
func fiberFloorMs(km float64) float64 { return 2 * km / 204190 * 1000 }

// parseCfL4 reads Cloudflare's cfL4 Server-Timing entry, for example
// cfL4;desc="?proto=TCP&rtt=15551&min_rtt=12096&rtt_var=5109&sent=23&recv=17&lost=0&retrans=0&...".
// RTT values are in microseconds.
func parseCfL4(h string) (l4Snapshot, bool) {
	i := strings.Index(h, "cfL4")
	if i < 0 {
		return l4Snapshot{}, false
	}
	rest := h[i:]
	j := strings.Index(rest, "desc=")
	if j < 0 {
		return l4Snapshot{}, false
	}
	desc := strings.TrimPrefix(rest[j+5:], "\"")
	if k := strings.IndexAny(desc, "\","); k >= 0 {
		desc = desc[:k]
	}
	q, err := url.ParseQuery(strings.TrimPrefix(desc, "?"))
	if err != nil || q.Get("rtt") == "" {
		return l4Snapshot{}, false
	}
	f := func(k string) float64 { v, _ := strconv.ParseFloat(q.Get(k), 64); return v }
	return l4Snapshot{
		rtt: f("rtt") / 1000, minRTT: f("min_rtt") / 1000, rttVar: f("rtt_var") / 1000,
		sent: int64(f("sent")), retrans: int64(f("retrans")),
	}, true
}

func summarizeL4(list []*l4Snapshot) *ServerTCP {
	var st ServerTCP
	var rtts, vars []float64
	for _, s := range list {
		if s == nil {
			continue
		}
		st.Connections++
		st.Sent += s.sent
		st.Retrans += s.retrans
		rtts = append(rtts, s.rtt)
		vars = append(vars, s.rttVar)
		if s.minRTT > 0 && (st.MinRTT == 0 || s.minRTT < st.MinRTT) {
			st.MinRTT = s.minRTT
		}
	}
	if st.Connections == 0 {
		return nil
	}
	st.RTT = round3(percentile(rtts, 50))
	st.RTTVar = round3(percentile(vars, 50))
	st.MinRTT = round3(st.MinRTT)
	if st.Sent > 0 {
		st.RetransPct = float64(st.Retrans) / float64(st.Sent) * 100
	}
	return &st
}
