package main

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Route tracing: pings with a growing time-to-live. Each router on the way
// that drops a packet whose TTL ran out says so, and that reveals the path to
// the server hop by hop, with the round-trip time to each.

type Hop struct {
	TTL  int       `json:"ttl"`
	IP   string    `json:"ip"`
	Host string    `json:"host,omitempty"`
	RTTs []float64 `json:"rtts"` // ms; -1 = no answer
	Dest bool      `json:"dest,omitempty"`
}

const (
	routeMaxHops = 30
	routeTries   = 3
)

// Swappable so tests can stand in for the network.
var (
	probeTTL       = icmpProbeTTL
	routeSupported = icmpSupported
)

func traceRoute(ctx context.Context, dst net.IP) []Hop {
	if !routeSupported() || dst.To4() == nil {
		return nil
	}
	type answer struct {
		ttl, try int
		from     net.IP
		rtt      float64
		reached  bool
	}
	answers := make(chan answer, routeMaxHops*routeTries)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32) // at most 32 probes in flight
	for ttl := 1; ttl <= routeMaxHops; ttl++ {
		for try := 0; try < routeTries; try++ {
			wg.Add(1)
			go func(ttl, try int) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				from, rtt, reached, err := probeTTL(dst, ttl, 1500*time.Millisecond)
				a := answer{ttl: ttl, try: try, rtt: -1}
				if err == nil && from != nil {
					a.from, a.rtt, a.reached = from, float64(rtt.Microseconds())/1000, reached
				}
				answers <- a
			}(ttl, try)
			time.Sleep(4 * time.Millisecond) // spread out, so routers' rate limits drop fewer
		}
	}
	wg.Wait()
	close(answers)

	hops := make([]Hop, routeMaxHops)
	for i := range hops {
		hops[i] = Hop{TTL: i + 1, RTTs: []float64{-1, -1, -1}}
	}
	destAt := routeMaxHops
	for a := range answers {
		h := &hops[a.ttl-1]
		h.RTTs[a.try] = round3(a.rtt)
		if a.from != nil && h.IP == "" {
			h.IP = a.from.String()
		}
		if a.reached {
			h.Dest = true
			if a.ttl < destAt {
				destAt = a.ttl
			}
		}
	}
	hops = hops[:destAt]
	// Trim silent hops at the end if the destination never answered.
	if destAt == routeMaxHops {
		for len(hops) > 0 && hops[len(hops)-1].IP == "" {
			hops = hops[:len(hops)-1]
		}
	}

	// Names for the hops, where they have them (reverse DNS), in parallel.
	var nwg sync.WaitGroup
	for i := range hops {
		if hops[i].IP == "" {
			continue
		}
		nwg.Add(1)
		go func(h *Hop) {
			defer nwg.Done()
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if names, err := net.DefaultResolver.LookupAddr(c, h.IP); err == nil && len(names) > 0 {
				sort.Strings(names)
				h.Host = strings.TrimSuffix(names[0], ".")
			}
		}(&hops[i])
	}
	nwg.Wait()
	return hops
}
