// Package httpapi wires HTTP routes, middleware, handlers and SSE streaming
// for the squishy backend.
package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"gitlab.com/dalibo/squishy/internal/events"
	"gitlab.com/dalibo/squishy/internal/project"
	"gitlab.com/dalibo/squishy/internal/version"
)

type Deps struct {
	DB   *pgxpool.Pool
	Repo *project.Repo
	Bus  *events.Bus
	Log  zerolog.Logger
}

// Handler builds the mux. Mount at / (not at /api/v1) — the prefix is added
// on routes below.
func Handler(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(requestLogger(d.Log))
	r.Use(cors)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { okJSON(w, map[string]string{"status": "ok"}) })
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := d.DB.Ping(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		okJSON(w, map[string]string{"status": "ready"})
	})
	r.Get("/version", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]string{
			"version": version.Version, "commit": version.Commit, "built": version.BuildDate,
		})
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/projects", func(r chi.Router) {
			r.Post("/", d.createProject)
			r.Get("/", d.listProjects)
			r.Route("/{projectID}", func(r chi.Router) {
				r.Get("/", d.getProject)
				r.Put("/", d.updateProject)
				r.Delete("/", d.deleteProject)
				r.Get("/instances", d.listInstances)
				r.Post("/instances", d.createInstance)
			})
		})
		r.Route("/instances/{instanceID}", func(r chi.Router) {
			r.Get("/", d.getInstance)
			r.Put("/", d.updateInstance)
			r.Delete("/", d.deleteInstance)
			r.Post("/test-connection", d.testInstanceConnection)
			r.Post("/test-target-connection", d.testInstanceTargetConnection)
			r.Post("/rediscover", d.rediscoverInstance)
			r.Get("/migrations", d.listInstanceMigrations)
		})
		r.Route("/migrations/{migrationID}", func(r chi.Router) {
			r.Get("/", d.getMigration)
			r.Post("/inspect", d.inspectMigration)
			r.Post("/plan", d.planMigration)
			r.Get("/prerequisites", d.getPrerequisites)
			r.Post("/prerequisites/ack", d.ackPrerequisites)
			r.Get("/runs", d.listMigrationRuns)
			r.Post("/runs", d.startRun)
		})
		r.Route("/runs/{runID}", func(r chi.Router) {
			r.Get("/", d.getRun)
			r.Get("/steps", d.listSteps)
			r.Get("/steps/{stepID}/batches", d.listBatches)
			r.Post("/steps/{stepID}/play", d.playStep)
			r.Post("/steps/{stepID}/replay", d.replayStep)
			r.Get("/events", d.streamEvents)
			r.Post("/cancel", d.cancelRun)
			r.Post("/retry", d.retryRun)
			r.Post("/levels/{level}/play", d.playLevel)
			r.Post("/levels/{level}/replay", d.replayLevel)
		})
	})

	return r
}

// ---- middleware ----

func requestLogger(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.Info().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", ww.Status()).
				Dur("dur", time.Since(start)).
				Msg("http")
		})
	}
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- response helpers ----

func okJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func createdJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(body)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
