package main

import (
	"fmt"
	"net/http"
	"strings"
)

// localOnly lets through only requests meant for this program's own page.
//
//   - Host must be localhost, 127.0.0.1 or [::1] on our port. This stops DNS
//     rebinding: a website that points its own name at 127.0.0.1 is still
//     refused, because the browser sends that website's name as the Host.
//   - Origin, when the browser sends one, must be this page.
//   - The API refuses anything the browser marks as cross-site.
//
// Changes also need the X-Speedtest header (see guarded), which browsers do
// not send cross-site without permission this program never gives.
func localOnly(port int, next http.Handler) http.Handler {
	ok := map[string]bool{}
	for _, h := range []string{"localhost", "127.0.0.1", "[::1]"} {
		ok[fmt.Sprintf("%s:%d", h, port)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		if !ok[strings.ToLower(r.Host)] {
			http.Error(w, fmt.Sprintf("Speed Test only answers at http://localhost:%d/", port), http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "null" && !ok[strings.TrimPrefix(strings.ToLower(o), "http://")] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o == "null" && r.Method != http.MethodGet {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
