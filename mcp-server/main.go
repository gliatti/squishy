package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

var apiURL string

func main() {
	apiURL = getenv("SQUISHY_API_URL", "http://api:8080")
	port := getenv("PORT", "8000")

	s := server.NewMCPServer(
		"squishy",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithLogging(),
	)

	registerTools(s)

	httpServer := server.NewStreamableHTTPServer(s,
		server.WithHeartbeatInterval(30*time.Second),
		server.WithStateLess(true),
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", httpServer)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	log.Printf("squishy-mcp listening on :%s (backend=%s)", port, apiURL)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
