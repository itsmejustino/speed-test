package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"
)

// Cloudflare's speed test server tells each visitor what it sees: the visitor's
// public IP, network (ASN and name), approximate location from the IP, and the
// code of the data center ("colo") that answered. /locations lists where each
// data center is.

func (r *Run) fetchMeta() (ServerInfo, ClientInfo, *Setup) {
	var s ServerInfo
	var c ClientInfo
	cl := &http.Client{Transport: r.transport(), Timeout: 10 * time.Second}
	defer cl.Transport.(*http.Transport).CloseIdleConnections()

	// The first request opens the first connection: time each step of it.
	setup := &Setup{Protocol: "HTTP/1.1"}
	var connStart, tlsStart, wrote time.Time
	trace := &httptrace.ClientTrace{
		ConnectStart: func(string, string) { connStart = time.Now() },
		ConnectDone: func(_, _ string, err error) {
			if err == nil && !connStart.IsZero() {
				setup.TCPMs = ms(time.Since(connStart))
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			if err == nil && !tlsStart.IsZero() {
				setup.TLSMs = ms(time.Since(tlsStart))
				setup.TLSVersion = tls.VersionName(cs.Version)
			}
		},
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote = time.Now() },
		GotFirstResponseByte: func() {
			if !wrote.IsZero() {
				setup.TTFBMs = ms(time.Since(wrote))
			}
		},
	}
	metaCtx := httptrace.WithClientTrace(r.ctx, trace)

	if m, err := getJSONMap(metaCtx, cl, r.cfg.Server+"/meta", r.cfg.Server); err == nil {
		c.IP = str(m["clientIp"])
		c.ASN = int(num(m["asn"]))
		c.ISP = str(m["asOrganization"])
		c.City = str(m["city"])
		c.Region = str(m["region"])
		c.Country = str(m["country"])
		c.Postal = str(m["postalCode"])
		c.Lat, c.Lon = num(m["latitude"]), num(m["longitude"])
		c.HasGeo = m["latitude"] != nil && m["longitude"] != nil && !(c.Lat == 0 && c.Lon == 0)
		// "colo" used to be a code; it is now the whole data-center record.
		if co, ok := m["colo"].(map[string]any); ok {
			s.Colo = strings.ToUpper(str(co["iata"]))
			if co["lat"] != nil && co["lon"] != nil {
				s.Lat, s.Lon, s.HasGeo = num(co["lat"]), num(co["lon"]), true
				s.City, s.Country = str(co["city"]), str(co["cca2"])
			}
		} else {
			s.Colo = strings.ToUpper(str(m["colo"]))
		}
	} else {
		r.warn("The Cloudflare speed test service did not send your ISP and location (" + err.Error() + "), so distance and comparison are not available.")
		// Fallback: the same details come back as headers on any download.
		req, _ := http.NewRequestWithContext(r.ctx, "GET", r.cfg.Server+"/__down?bytes=0", nil)
		browserHeaders(req, r.cfg.Server)
		if resp, err := cl.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			h := resp.Header
			c.IP = h.Get("cf-meta-ip")
			c.ASN, _ = strconv.Atoi(h.Get("cf-meta-asn"))
			c.City = h.Get("cf-meta-city")
			c.Country = h.Get("cf-meta-country")
			c.Postal = h.Get("cf-meta-postalcode")
			if lat, err1 := strconv.ParseFloat(h.Get("cf-meta-latitude"), 64); err1 == nil {
				if lon, err2 := strconv.ParseFloat(h.Get("cf-meta-longitude"), 64); err2 == nil {
					c.Lat, c.Lon, c.HasGeo = lat, lon, true
				}
			}
			s.Colo = strings.ToUpper(h.Get("cf-meta-colo"))
			if s.Colo == "" {
				if ray := h.Get("cf-ray"); strings.Contains(ray, "-") {
					s.Colo = strings.ToUpper(ray[strings.LastIndex(ray, "-")+1:])
				}
			}
		}
		// Country and data center from the trace page, which every Cloudflare site has.
		if c.Country == "" || s.Colo == "" {
			req, _ := http.NewRequestWithContext(r.ctx, "GET", r.cfg.Server+"/cdn-cgi/trace", nil)
			browserHeaders(req, r.cfg.Server)
			if resp, err := cl.Do(req); err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
				_ = resp.Body.Close()
				for _, line := range strings.Split(string(b), "\n") {
					k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
					switch {
					case k == "ip" && c.IP == "":
						c.IP = v
					case k == "loc" && c.Country == "":
						c.Country = v
					case k == "colo" && s.Colo == "":
						s.Colo = strings.ToUpper(v)
					}
				}
			}
		}
	}

	if s.Colo != "" && !s.HasGeo {
		if list, err := getJSONList(r.ctx, cl, r.cfg.Server+"/locations", r.cfg.Server); err == nil {
			for _, it := range list {
				m, ok := it.(map[string]any)
				if !ok || !strings.EqualFold(str(m["iata"]), s.Colo) {
					continue
				}
				s.Lat, s.Lon, s.HasGeo = num(m["lat"]), num(m["lon"]), true
				s.City = str(m["city"])
				s.Country = str(m["cca2"])
				break
			}
		}
		if !s.HasGeo {
			if k, ok := knownColos[s.Colo]; ok {
				s.Lat, s.Lon, s.City, s.Country, s.HasGeo = k.lat, k.lon, k.city, k.cc, true
			}
		}
	}
	if setup.TCPMs == 0 {
		setup = nil
	}
	return s, c, setup
}

func getJSONMap(ctx context.Context, c *http.Client, url, site string) (map[string]any, error) {
	var m map[string]any
	return m, getJSON(ctx, c, url, site, &m)
}

func getJSONList(ctx context.Context, c *http.Client, url, site string) ([]any, error) {
	var l []any
	return l, getJSON(ctx, c, url, site, &l)
}

// speed.cloudflare.com answers /meta only to its own page: a request without
// that page as the referrer gets 403. These are the headers the page sends.
func browserHeaders(req *http.Request, site string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36 SpeedTest-native/1.1")
	req.Header.Set("Referer", site+"/")
	req.Header.Set("Origin", site)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
}

func getJSON(ctx context.Context, c *http.Client, url, site string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	browserHeaders(req, site)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return &testError{"HTTP " + strconv.Itoa(resp.StatusCode)}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
}

// Used only if /locations cannot be read. Airport coordinates, which is where
// Cloudflare names its data centers.
type colo struct {
	lat, lon float64
	city, cc string
}

var knownColos = map[string]colo{
	"ABQ": {35.04, -106.61, "Albuquerque", "US"}, "ANC": {61.17, -150.00, "Anchorage", "US"},
	"ATL": {33.64, -84.43, "Atlanta", "US"}, "AUS": {30.19, -97.67, "Austin", "US"},
	"BNA": {36.12, -86.68, "Nashville", "US"}, "BOS": {42.36, -71.01, "Boston", "US"},
	"BUF": {42.94, -78.73, "Buffalo", "US"}, "CLT": {35.21, -80.94, "Charlotte", "US"},
	"CMH": {39.99, -82.89, "Columbus", "US"}, "DEN": {39.86, -104.67, "Denver", "US"},
	"DFW": {32.90, -97.04, "Dallas", "US"}, "DTW": {42.21, -83.35, "Detroit", "US"},
	"EWR": {40.69, -74.17, "Newark", "US"}, "HNL": {21.32, -157.92, "Honolulu", "US"},
	"IAD": {38.95, -77.46, "Ashburn", "US"}, "IAH": {29.99, -95.34, "Houston", "US"},
	"IND": {39.72, -86.29, "Indianapolis", "US"}, "JAX": {30.49, -81.69, "Jacksonville", "US"},
	"LAS": {36.08, -115.15, "Las Vegas", "US"}, "LAX": {33.94, -118.41, "Los Angeles", "US"},
	"MCI": {39.30, -94.71, "Kansas City", "US"}, "MEM": {35.04, -89.98, "Memphis", "US"},
	"MIA": {25.79, -80.29, "Miami", "US"}, "MSP": {44.88, -93.22, "Minneapolis", "US"},
	"OKC": {35.39, -97.60, "Oklahoma City", "US"}, "OMA": {41.30, -95.89, "Omaha", "US"},
	"ORD": {41.98, -87.90, "Chicago", "US"}, "PDX": {45.59, -122.60, "Portland", "US"},
	"PHL": {39.87, -75.24, "Philadelphia", "US"}, "PHX": {33.43, -112.01, "Phoenix", "US"},
	"PIT": {40.49, -80.23, "Pittsburgh", "US"}, "RIC": {37.51, -77.32, "Richmond", "US"},
	"SAN": {32.73, -117.19, "San Diego", "US"}, "SAT": {29.53, -98.47, "San Antonio", "US"},
	"SEA": {47.45, -122.31, "Seattle", "US"}, "SFO": {37.62, -122.38, "San Francisco", "US"},
	"SJC": {37.36, -121.93, "San Jose", "US"}, "SLC": {40.79, -111.98, "Salt Lake City", "US"},
	"SMF": {38.70, -121.59, "Sacramento", "US"}, "STL": {38.75, -90.37, "St. Louis", "US"},
	"TPA": {27.98, -82.53, "Tampa", "US"}, "YYC": {51.13, -114.01, "Calgary", "CA"},
	"YUL": {45.47, -73.74, "Montréal", "CA"}, "YVR": {49.19, -123.18, "Vancouver", "CA"},
	"YYZ": {43.68, -79.63, "Toronto", "CA"}, "MEX": {19.44, -99.07, "Mexico City", "MX"},
	"GRU": {-23.43, -46.47, "São Paulo", "BR"}, "LHR": {51.47, -0.45, "London", "GB"},
	"AMS": {52.31, 4.77, "Amsterdam", "NL"}, "FRA": {50.04, 8.56, "Frankfurt", "DE"},
	"CDG": {49.01, 2.55, "Paris", "FR"}, "NRT": {35.77, 140.39, "Tokyo", "JP"},
	"SIN": {1.36, 103.99, "Singapore", "SG"}, "SYD": {-33.95, 151.18, "Sydney", "AU"},
}
