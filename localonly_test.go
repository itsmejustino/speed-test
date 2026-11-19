package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalOnly(t *testing.T) {
	h := localOnly(8765, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	cases := []struct {
		name, method, host, path string
		hdr                      map[string]string
		want                     int
	}{
		{"own page", "GET", "localhost:8765", "/", nil, 200},
		{"loopback ip", "GET", "127.0.0.1:8765", "/api/history", nil, 200},
		{"ipv6 loopback", "GET", "[::1]:8765", "/api/history", nil, 200},
		{"same-origin post", "POST", "localhost:8765", "/api/start", map[string]string{"Origin": "http://localhost:8765", "Sec-Fetch-Site": "same-origin"}, 200},
		{"rebinding", "GET", "evil.example:8765", "/api/history", nil, 403},
		{"rebinding, no port", "GET", "localhost", "/api/history", nil, 403},
		{"other port", "GET", "localhost:9999", "/", nil, 403},
		{"foreign origin", "POST", "localhost:8765", "/api/settings", map[string]string{"Origin": "https://evil.example"}, 403},
		{"null origin post", "POST", "localhost:8765", "/api/settings", map[string]string{"Origin": "null"}, 403},
		{"cross-site api read", "GET", "localhost:8765", "/api/history", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"link from another site opens the page", "GET", "localhost:8765", "/", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://"+c.host+c.path, nil)
		r.Host = c.host
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: no X-Frame-Options", c.name)
		}
	}
}
