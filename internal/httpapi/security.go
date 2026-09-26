package httpapi

import (
	"crypto/subtle"
	"net/http"
	"net/url"
)

// cors answers CORS only for the configured origins: the web UI reaches
// the API same-origin through the Vite proxy, so no other site may read
// API responses from a victim's browser.
func cors(allowed map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// originGuard refuses state-changing requests sent by a browser from a
// foreign origin. A cross-site form or text/plain fetch needs no CORS
// preflight, so CORS alone does not stop it from creating instances or
// starting runs (CSRF). Requests without an Origin header (curl, the MCP
// server, the e2e suite) are not browser cross-site requests and pass.
func originGuard(allowed map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if origin == "" || allowed[origin] || sameHost(origin, r.Host) {
				next.ServeHTTP(w, r)
				return
			}
			errJSON(w, http.StatusForbidden, "cross-origin request from "+origin+" refused (add it to SQUISHY_ALLOWED_ORIGINS)")
		})
	}
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && u.Host == host
}

// requireToken enforces `Authorization: Bearer <token>` when a token is
// configured. An empty token leaves the API open (local single-user use).
func requireToken(token string) func(http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="squishy"`)
				errJSON(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
