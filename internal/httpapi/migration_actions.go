package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"gitlab.com/dalibo/squishy/internal/connection"
	"gitlab.com/dalibo/squishy/internal/dialects"
	_ "gitlab.com/dalibo/squishy/internal/dialects/db2"      // registers db2 + db2zos
	_ "gitlab.com/dalibo/squishy/internal/dialects/mysql"    // registers mysql + mariadb
	_ "gitlab.com/dalibo/squishy/internal/dialects/oracle"   // registers oracle
	_ "gitlab.com/dalibo/squishy/internal/dialects/postgres" // registers postgres (target)
	"gitlab.com/dalibo/squishy/internal/inspect"
	"gitlab.com/dalibo/squishy/internal/planner"
	"gitlab.com/dalibo/squishy/internal/project"
	"gitlab.com/dalibo/squishy/internal/translate"
)

// inspectSourceForSchema runs the dialect-aware introspector on `db`, scoped to
// the given source schema/database name. The returned timings are non-nil even
// on error so callers can still surface partial timings (e.g. how far the walk
// got before failing).
func inspectSourceForSchema(ctx context.Context, db *sql.DB, kind, sourceSchema string, log zerolog.Logger) (*inspect.SourceSchema, *inspect.InspectTimings, error) {
	switch kind {
	case "mysql", "mariadb":
		return inspect.InspectMySQL(ctx, db, sourceSchema, log)
	case "oracle", "oracle19":
		return inspect.InspectOracle(ctx, db, sourceSchema, log)
	case "db2", "db2zos":
		return inspect.InspectDB2(ctx, db, sourceSchema, log)
	}
	return nil, nil, errors.New("unsupported source kind: " + kind)
}

// inspectMigration introspects the source schema of a migration's instance and
// returns the SourceSchema JSON. Does not persist — that happens in plan.
func (d *Deps) inspectMigration(w http.ResponseWriter, r *http.Request) {
	mig, inst, err := d.loadMigrationWithInstance(r.Context(), chi.URLParam(r, "migrationID"))
	if err != nil {
		writeMigErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	db, err := openSourceForInstance(ctx, inst)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	defer db.Close()
	schema, _, err := inspectSourceForSchema(ctx, db, inst.Kind, mig.SourceSchemaName, d.Log)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, schema)
}

type planReq struct {
	Options map[string]any `json:"options"`
}

// planTimings is the per-phase wall-clock breakdown of a planMigration call.
// Durations are serialized as integer milliseconds via MarshalJSON so the
// payload stays human-readable in API responses and logs.
type planTimings struct {
	Inspect      *inspect.InspectTimings
	ProbeExts    time.Duration
	Parse        time.Duration
	Translate    time.Duration
	PlannerBuild time.Duration
	Persist      time.Duration
	Total        time.Duration
}

func (t *planTimings) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Inspect        *inspect.InspectTimings `json:"inspect"`
		ProbeExtsMs    int64                   `json:"probe_exts_ms"`
		ParseMs        int64                   `json:"parse_ms"`
		TranslateMs    int64                   `json:"translate_ms"`
		PlannerBuildMs int64                   `json:"planner_build_ms"`
		PersistMs      int64                   `json:"persist_ms"`
		TotalMs        int64                   `json:"total_ms"`
	}{
		Inspect:        t.Inspect,
		ProbeExtsMs:    t.ProbeExts.Milliseconds(),
		ParseMs:        t.Parse.Milliseconds(),
		TranslateMs:    t.Translate.Milliseconds(),
		PlannerBuildMs: t.PlannerBuild.Milliseconds(),
		PersistMs:      t.Persist.Milliseconds(),
		TotalMs:        t.Total.Milliseconds(),
	})
}

// planMigration inspects the source, runs the dialect parser, translates to PG,
// and upserts the migration row with the freshly-computed plan.
func (d *Deps) planMigration(w http.ResponseWriter, r *http.Request) {
	mig, inst, err := d.loadMigrationWithInstance(r.Context(), chi.URLParam(r, "migrationID"))
	if err != nil {
		writeMigErr(w, err)
		return
	}
	var req planReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	timings := &planTimings{}
	totalStart := time.Now()
	defer func() { timings.Total = time.Since(totalStart) }()

	db, err := openSourceForInstance(ctx, inst)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	defer db.Close()

	schema, inspectTimings, err := inspectSourceForSchema(ctx, db, inst.Kind, mig.SourceSchemaName, d.Log)
	timings.Inspect = inspectTimings
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Probe extensions on the instance's admin PG (the per-migration target
	// DB may not exist yet for the dedicated_db strategy).
	probeStart := time.Now()
	var targetExts []string
	if pool, err := connection.OpenPostgres(ctx, connection.Params{
		Kind: "postgres", Host: inst.TargetHost, Port: inst.TargetPort,
		Database: inst.TargetDatabase, Username: inst.TargetUsername,
		Password: inst.TargetPassword, SSLMode: inst.TargetSSLMode,
	}); err == nil {
		rows, err := pool.Query(ctx, `SELECT extname FROM pg_extension`)
		if err == nil {
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err == nil {
					targetExts = append(targetExts, n)
				}
			}
			rows.Close()
		}
		pool.Close()
	}
	timings.ProbeExts = time.Since(probeStart)

	dia, err := dialects.Get(inst.Kind)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	parseStart := time.Now()
	var allDDL []byte
	for _, snaps := range [][]inspect.ObjectSnapshot{
		schema.Tables, schema.Views, schema.Triggers, schema.Procedures, schema.Functions, schema.Events,
	} {
		for _, s := range snaps {
			allDDL = append(allDDL, s.DDL...)
			allDDL = append(allDDL, ';', '\n', '\n')
		}
	}
	stmts, parseErr := dia.Parse(string(allDDL))
	timings.Parse = time.Since(parseStart)

	translateStart := time.Now()
	result := translate.Translate(stmts, translate.Options{
		TargetSchema:     mig.TargetSchemaName,
		SourceKind:       dialects.Kind(inst.Kind),
		TargetExtensions: targetExts,
	})
	if parseErr != nil {
		result.Warnings = append(result.Warnings, translate.Warning{
			Kind: "parse", Message: parseErr.Error(),
		})
	}
	timings.Translate = time.Since(translateStart)

	// Build the step DAG. Prepend a create_target_db step iff the instance's
	// strategy is dedicated_db; the worker will idempotently CREATE DATABASE
	// before anything else runs.
	plannerStart := time.Now()
	pl := planner.Build(schema, result, planner.BuildOptions{
		TargetSchema:   mig.TargetSchemaName,
		TargetDBName:   mig.TargetDBName,
		CreateTargetDB: inst.TargetStrategy == "dedicated_db",
	})
	timings.PlannerBuild = time.Since(plannerStart)

	dataPlan, _ := json.Marshal(map[string]any{"steps": pl.Steps})

	sourceJSON, _ := json.Marshal(schema)
	targetJSON, _ := json.Marshal(result.Plan)
	expsJSON, _ := json.Marshal(result.Explanations)
	warnsJSON, _ := json.Marshal(result.Warnings)
	mapsJSON, _ := json.Marshal(result.TypeMappings)
	prereqsJSON, _ := json.Marshal(result.Prerequisites)
	optsJSON, _ := json.Marshal(req.Options)

	updated := &project.Migration{
		ID:               mig.ID,
		InstanceID:       inst.ID,
		SourceSchemaName: mig.SourceSchemaName,
		TargetDBName:     mig.TargetDBName,
		TargetSchemaName: mig.TargetSchemaName,
		SourceSchema:     sourceJSON,
		TargetPlan:       targetJSON,
		DDLScript:        result.DDLScript,
		DDLPostScript:    result.DDLPostCopy,
		DataPlan:         dataPlan,
		TypeMappings:     mapsJSON,
		Explanations:     expsJSON,
		Warnings:         warnsJSON,
		Prerequisites:    prereqsJSON,
		Options:          optsJSON,
	}
	persistStart := time.Now()
	if err := d.Repo.UpsertPlannedMigration(r.Context(), updated); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	timings.Persist = time.Since(persistStart)

	// Stop the deferred Total timer early so the recap log reflects the
	// effective end-to-end duration of the work, not the response write.
	timings.Total = time.Since(totalStart)
	inspectTotal := time.Duration(0)
	if timings.Inspect != nil {
		inspectTotal = timings.Inspect.Total
	}
	d.Log.Info().
		Str("migration_id", mig.ID.String()).
		Str("dialect", inst.Kind).
		Dur("total", timings.Total).
		Dur("inspect", inspectTotal).
		Dur("probe_exts", timings.ProbeExts).
		Dur("parse", timings.Parse).
		Dur("translate", timings.Translate).
		Dur("planner_build", timings.PlannerBuild).
		Dur("persist", timings.Persist).
		Int("stmts", len(stmts)).
		Msg("plan done")

	createdJSON(w, map[string]any{
		"migration_id":    updated.ID,
		"status":          updated.Status,
		"ddl_script":      result.DDLScript,
		"ddl_post_script": result.DDLPostCopy,
		"explanations":    result.Explanations,
		"type_mappings":   result.TypeMappings,
		"warnings":        result.Warnings,
		"prerequisites":   result.Prerequisites,
		"stmts_parsed":    len(stmts),
		"timings":         timings,
	})
}

// loadMigrationWithInstance resolves migrationID → (Migration, Instance).
func (d *Deps) loadMigrationWithInstance(ctx context.Context, raw string) (*project.Migration, *project.Instance, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, nil, errBadID
	}
	mig, err := d.Repo.GetMigration(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	inst, err := d.Repo.GetInstance(ctx, mig.InstanceID)
	if err != nil {
		return nil, nil, err
	}
	return mig, inst, nil
}

var errBadID = errors.New("invalid migrationID")

func writeMigErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadID):
		errJSON(w, http.StatusBadRequest, "invalid migrationID")
	case errors.Is(err, project.ErrNotFound):
		errJSON(w, http.StatusNotFound, "migration not found")
	default:
		errJSON(w, http.StatusInternalServerError, err.Error())
	}
}
