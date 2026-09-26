package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestCORSOnlyForAllowedOrigins(t *testing.T) {
	h := cors(map[string]bool{"http://localhost:5173": true})(okHandler())
	for _, c := range []struct{ origin, want string }{
		{"http://localhost:5173", "http://localhost:5173"},
		{"https://evil.example", ""},
		{"", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != c.want {
			t.Errorf("origin %q: Access-Control-Allow-Origin = %q, want %q", c.origin, got, c.want)
		}
	}
}

func TestOriginGuard(t *testing.T) {
	h := originGuard(map[string]bool{"http://localhost:5173": true})(okHandler())
	for _, c := range []struct {
		name, method, origin, host string
		want                       int
	}{
		{"no origin (curl, MCP)", http.MethodPost, "", "api:8080", http.StatusOK},
		{"allowed origin", http.MethodPost, "http://localhost:5173", "api:8080", http.StatusOK},
		{"same host", http.MethodPut, "http://api:8080", "api:8080", http.StatusOK},
		{"foreign origin POST", http.MethodPost, "https://evil.example", "api:8080", http.StatusForbidden},
		{"foreign origin DELETE", http.MethodDelete, "https://evil.example", "api:8080", http.StatusForbidden},
		{"foreign origin GET", http.MethodGet, "https://evil.example", "api:8080", http.StatusOK},
		{"null origin POST", http.MethodPost, "null", "api:8080", http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/api/v1/projects", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d", w.Code, c.want)
			}
		})
	}
}

func TestRequireToken(t *testing.T) {
	open := requireToken("")(okHandler())
	w := httptest.NewRecorder()
	open.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("no token configured: status = %d, want 200", w.Code)
	}

	h := requireToken("s3cret")(okHandler())
	for _, c := range []struct {
		auth string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"s3cret", http.StatusUnauthorized},
		{"Bearer s3cret", http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("Authorization %q: status = %d, want %d", c.auth, w.Code, c.want)
		}
	}
}
