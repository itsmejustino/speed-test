package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Comparison with other people's speed tests, from Cloudflare Radar: the last
// 90 days of speed.cloudflare.com results, for your ISP in your country and for
// your country as a whole. Radar needs a (free) Cloudflare API token.

type RadarSummary struct {
	Down          float64 `json:"down"`
	Up            float64 `json:"up"`
	LatencyIdle   float64 `json:"latencyIdle"`
	LatencyLoaded float64 `json:"latencyLoaded"`
	JitterIdle    float64 `json:"jitterIdle"`
	JitterLoaded  float64 `json:"jitterLoaded"`
	PacketLoss    float64 `json:"packetLoss"`
}

type Compare struct {
	Country string        `json:"country"`
	ASN     int           `json:"asn"`
	ISP     string        `json:"isp"`
	ISPMed  *RadarSummary `json:"ispMedian,omitempty"`
	CtryMed *RadarSummary `json:"countryMedian,omitempty"`
	// Share of tests (0-100) that were slower than yours.
	DownRankISP  *float64 `json:"downRankIsp,omitempty"`
	UpRankISP    *float64 `json:"upRankIsp,omitempty"`
	DownRankCtry *float64 `json:"downRankCountry,omitempty"`
	UpRankCtry   *float64 `json:"upRankCountry,omitempty"`
	Error        string   `json:"error,omitempty"`
}

const radarBase = "https://api.cloudflare.com/client/v4/radar/quality/speed/"

func compareWithRadar(ctx context.Context, token string, c ClientInfo, down, up, idle float64) *Compare {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cl := &http.Client{Timeout: 15 * time.Second}
	out := &Compare{Country: c.Country, ASN: c.ASN, ISP: c.ISP}

	isp := url.Values{"asn": {strconv.Itoa(c.ASN)}, "location": {c.Country}, "format": {"JSON"}}
	ctry := url.Values{"location": {c.Country}, "format": {"JSON"}}

	var firstErr string
	if s, err := radarSummary(ctx, cl, token, isp); err == nil {
		out.ISPMed = s
	} else {
		firstErr = err.Error()
	}
	if s, err := radarSummary(ctx, cl, token, ctry); err == nil {
		out.CtryMed = s
	} else if firstErr == "" {
		firstErr = err.Error()
	}
	if out.ISPMed == nil && out.CtryMed == nil {
		out.Error = firstErr
		return out
	}
	out.DownRankISP, out.UpRankISP = radarRanks(ctx, cl, token, isp, down, up)
	out.DownRankCtry, out.UpRankCtry = radarRanks(ctx, cl, token, ctry, down, up)
	return out
}

func radarGet(ctx context.Context, cl *http.Client, token, path string, q url.Values) (map[string]any, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", radarBase+path+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, &testError{"Could not reach the Cloudflare Radar API"}
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m) // an unreadable body is handled as "no data" below
	if resp.StatusCode != 200 || m == nil {
		code, msg := radarError(m)
		switch {
		case code == 9106 || (resp.StatusCode == 400 && code == 0):
			return nil, &testError{"the saved token is not in the right form. In Settings, paste only the token itself (no \"Bearer\", no spaces)"}
		case resp.StatusCode == 401 || code == 10000:
			return nil, &testError{"the Cloudflare API did not recognize the token. It may have been copied incompletely, deleted or expired; create a new one and paste it in Settings"}
		case resp.StatusCode == 403:
			return nil, &testError{"the token works but cannot read Radar data. Edit it in the Cloudflare dashboard and give it Account \u2192 Radar \u2192 Read"}
		case resp.StatusCode == 429:
			return nil, &testError{"the Cloudflare Radar API is rate-limiting requests; try again later"}
		}
		detail := "HTTP " + strconv.Itoa(resp.StatusCode)
		if msg != "" {
			detail += ": " + msg
		}
		return nil, &testError{"the Cloudflare Radar API answered " + detail}
	}
	res, _ := m["result"].(map[string]any)
	if res == nil {
		return nil, &testError{"the Cloudflare Radar API returned no data"}
	}
	return res, nil
}

func radarError(m map[string]any) (int, string) {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return 0, ""
	}
	e, _ := errs[0].(map[string]any)
	return int(num(e["code"])), str(e["message"])
}

// checkRadarToken makes one small Radar request with the token.
func checkRadarToken(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := radarSummary(ctx, &http.Client{Timeout: 15 * time.Second}, token, url.Values{"location": {"US"}, "format": {"JSON"}})
	return err
}

func radarSummary(ctx context.Context, cl *http.Client, token string, q url.Values) (*RadarSummary, error) {
	res, err := radarGet(ctx, cl, token, "summary", q)
	if err != nil {
		return nil, err
	}
	s, _ := res["summary_0"].(map[string]any)
	if s == nil {
		return nil, &testError{"the Cloudflare Radar API returned no summary"}
	}
	return &RadarSummary{
		Down: num(s["bandwidthDownload"]), Up: num(s["bandwidthUpload"]),
		LatencyIdle: num(s["latencyIdle"]), LatencyLoaded: num(s["latencyLoaded"]),
		JitterIdle: num(s["jitterIdle"]), JitterLoaded: num(s["jitterLoaded"]),
		PacketLoss: num(s["packetLoss"]),
	}, nil
}

// Where your speed falls among everyone's, from Radar's histogram of results.
func radarRanks(ctx context.Context, cl *http.Client, token string, base url.Values, down, up float64) (*float64, *float64) {
	const bucket = 5.0 // Mbps per bucket
	q := url.Values{}
	for k, v := range base {
		q[k] = v
	}
	q.Set("metricGroup", "BANDWIDTH")
	q.Set("bucketSize", "5")
	res, err := radarGet(ctx, cl, token, "histogram", q)
	if err != nil {
		return nil, nil
	}
	h, _ := res["histogram_0"].(map[string]any)
	if h == nil {
		return nil, nil
	}
	mins := floats(h["bucketMin"])
	rank := func(key string, mine float64) *float64 {
		counts := floats(h[key])
		if len(counts) == 0 {
			return nil
		}
		total, below := 0.0, 0.0
		for i, n := range counts {
			lo := float64(i) * bucket
			if i < len(mins) {
				lo = mins[i]
			}
			hi := lo + bucket
			if i+1 < len(mins) {
				hi = mins[i+1]
			}
			total += n
			switch {
			case mine >= hi:
				below += n
			case mine > lo && hi > lo:
				below += n * (mine - lo) / (hi - lo)
			}
		}
		if total == 0 {
			return nil
		}
		v := below / total * 100
		return &v
	}
	return rank("bandwidthDownload", down), rank("bandwidthUpload", up)
}

func floats(v any) []float64 {
	a, _ := v.([]any)
	out := make([]float64, len(a))
	for i, x := range a {
		out[i] = num(x)
	}
	return out
}
