package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.com/dalibo/squishy/internal/connection"
	"gitlab.com/dalibo/squishy/internal/discover"
	"gitlab.com/dalibo/squishy/internal/project"
)

// ---- instances ----

type createInstanceReq struct {
	Name string `json:"name"`

	// Source DSN.
	Kind     string         `json:"kind"`
	Host     string         `json:"host"`
	Port     int            `json:"port"`
	Database string         `json:"database"`
	Username string         `json:"username"`
	Password string         `json:"password"`
	SSLMode  string         `json:"ssl_mode"`
	Params   map[string]any `json:"params"`

	// Mapping strategy + target DSN.
	TargetStrategy string         `json:"target_strategy"`
	TargetHost     string         `json:"target_host"`
	TargetPort     int            `json:"target_port"`
	TargetDatabase string         `json:"target_database"`
	TargetUsername string         `json:"target_username"`
	TargetPassword string         `json:"target_password"`
	TargetSSLMode  string         `json:"target_ssl_mode"`
	TargetParams   map[string]any `json:"target_params"`
	TargetCreateDB bool           `json:"target_create_db"`
}

func (d *Deps) createInstance(w http.ResponseWriter, r *http.Request) {
	projectID, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid projectID")
		return
	}
	var req createInstanceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}
	if !validKind(req.Kind) {
		errJSON(w, http.StatusBadRequest, "kind must be one of mysql|mariadb|oracle|oracle19")
		return
	}
	if req.TargetStrategy != "dedicated_db" && req.TargetStrategy != "dedicated_schema" {
		errJSON(w, http.StatusBadRequest, "target_strategy must be dedicated_db|dedicated_schema")
		return
	}
	if strings.TrimSpace(req.TargetHost) == "" || req.TargetPort == 0 {
		errJSON(w, http.StatusBadRequest, "target_host and target_port required")
		return
	}
	if strings.TrimSpace(req.TargetDatabase) == "" {
		errJSON(w, http.StatusBadRequest, "target_database required (admin DB for dedicated_db, host DB for dedicated_schema)")
		return
	}
	if strings.TrimSpace(req.TargetUsername) == "" {
		errJSON(w, http.StatusBadRequest, "target_username required")
		return
	}

	inst := &project.Instance{
		ProjectID: projectID, Name: req.Name, Kind: req.Kind,
		Host: req.Host, Port: req.Port, Database: req.Database,
		Username: req.Username, Password: req.Password, SSLMode: req.SSLMode,
		Params:         req.Params,
		TargetStrategy: req.TargetStrategy,
		TargetHost:     req.TargetHost,
		TargetPort:     req.TargetPort,
		TargetDatabase: req.TargetDatabase,
		TargetUsername: req.TargetUsername,
		TargetPassword: req.TargetPassword,
		TargetSSLMode:  req.TargetSSLMode,
		TargetParams:   req.TargetParams,
		TargetCreateDB: req.TargetCreateDB,
	}
	if err := d.Repo.CreateInstance(r.Context(), inst); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := map[string]any{"instance": inst}

	// If the user opted into auto-create, ensure target_database exists. The
	// helper is idempotent: it skips when the DB is already there.
	if inst.TargetCreateDB {
		if err := ensureTargetDatabase(r.Context(), inst); err != nil {
			resp["target_create_db_error"] = err.Error()
		}
	}

	migrations, derr := d.discoverAndDraft(r.Context(), inst)
	resp["migrations"] = migrations
	if derr != nil {
		resp["discover_error"] = derr.Error()
	}
	createdJSON(w, resp)
}

func (d *Deps) listInstances(w http.ResponseWriter, r *http.Request) {
	projectID, err := uuid.Parse(chi.URLParam(r, "projectID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid projectID")
		return
	}
	xs, err := d.Repo.ListInstances(r.Context(), projectID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"instances": xs})
}

func (d *Deps) getInstance(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "instanceID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid instanceID")
		return
	}
	inst, err := d.Repo.GetInstance(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "instance not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	migrations, err := d.Repo.ListMigrationsByInstance(r.Context(), id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"instance": inst, "migrations": migrations})
}

// updateInstanceReq mirrors createInstanceReq minus project_id (immutable) and
// kind (immutable — changing the dialect would invalidate planned migrations).
type updateInstanceReq struct {
	Name string `json:"name"`

	Host     string         `json:"host"`
	Port     int            `json:"port"`
	Database string         `json:"database"`
	Username string         `json:"username"`
	Password string         `json:"password"` // empty = keep existing
	SSLMode  string         `json:"ssl_mode"`
	Params   map[string]any `json:"params"`

	TargetStrategy string         `json:"target_strategy"`
	TargetHost     string         `json:"target_host"`
	TargetPort     int            `json:"target_port"`
	TargetDatabase string         `json:"target_database"`
	TargetUsername string         `json:"target_username"`
	TargetPassword string         `json:"target_password"` // empty = keep existing
	TargetSSLMode  string         `json:"target_ssl_mode"`
	TargetParams   map[string]any `json:"target_params"`
	TargetCreateDB bool           `json:"target_create_db"`
}

func (d *Deps) updateInstance(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "instanceID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid instanceID")
		return
	}
	existing, err := d.Repo.GetInstance(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "instance not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req updateInstanceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}
	if req.TargetStrategy != "dedicated_db" && req.TargetStrategy != "dedicated_schema" {
		errJSON(w, http.StatusBadRequest, "target_strategy must be dedicated_db|dedicated_schema")
		return
	}
	if strings.TrimSpace(req.TargetHost) == "" || req.TargetPort == 0 ||
		strings.TrimSpace(req.TargetDatabase) == "" || strings.TrimSpace(req.TargetUsername) == "" {
		errJSON(w, http.StatusBadRequest, "target_host / target_port / target_database / target_username required")
		return
	}

	// Carry-over: kind, project_id, id, created_at are immutable.
	updated := *existing
	updated.Name = req.Name
	updated.Host = req.Host
	updated.Port = req.Port
	updated.Database = req.Database
	updated.Username = req.Username
	updated.Password = req.Password // empty preserves existing via repo SQL
	updated.SSLMode = req.SSLMode
	updated.Params = req.Params
	updated.TargetStrategy = req.TargetStrategy
	updated.TargetHost = req.TargetHost
	updated.TargetPort = req.TargetPort
	updated.TargetDatabase = req.TargetDatabase
	updated.TargetUsername = req.TargetUsername
	updated.TargetPassword = req.TargetPassword // empty preserves existing
	updated.TargetSSLMode = req.TargetSSLMode
	updated.TargetParams = req.TargetParams
	updated.TargetCreateDB = req.TargetCreateDB

	if err := d.Repo.UpdateInstance(r.Context(), &updated); err != nil {
		if errors.Is(err, project.ErrNotFound) {
			errJSON(w, http.StatusNotFound, "instance not found")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-fetch so the response reflects the persisted state (including the
	// password we left untouched).
	final, err := d.Repo.GetInstance(r.Context(), id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if updated.TargetCreateDB {
		if err := ensureTargetDatabase(r.Context(), final); err != nil {
			okJSON(w, map[string]any{"instance": final, "target_create_db_error": err.Error()})
			return
		}
	}
	okJSON(w, final)
}

func (d *Deps) deleteInstance(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "instanceID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid instanceID")
		return
	}
	if err := d.Repo.DeleteInstance(r.Context(), id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testInstanceConnection probes the SOURCE super-user DSN.
func (d *Deps) testInstanceConnection(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "instanceID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid instanceID")
		return
	}
	inst, err := d.Repo.GetInstance(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "instance not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	params := map[string]string{}
	for k, v := range inst.Params {
		params[k] = toString(v)
	}
	res := connection.Test(ctx, connection.Params{
		Kind: inst.Kind, Host: inst.Host, Port: inst.Port, Database: inst.Database,
		Username: inst.Username, Password: inst.Password, SSLMode: inst.SSLMode, Extra: params,
	})
	okJSON(w, res)
}

// testInstanceTargetConnection probes the per-instance TARGET PG DSN. If the
// instance has target_create_db=true and the DB doesn't exist yet, squishy
// transparently creates it via the PG default DB ("postgres") and re-tests.
func (d *Deps) testInstanceTargetConnection(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "instanceID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid instanceID")
		return
	}
	inst, err := d.Repo.GetInstance(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "instance not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	params := map[string]string{}
	for k, v := range inst.TargetParams {
		params[k] = toString(v)
	}
	tgtParams := connection.Params{
		Kind: "postgres", Host: inst.TargetHost, Port: inst.TargetPort,
		Database: inst.TargetDatabase, Username: inst.TargetUsername,
		Password: inst.TargetPassword, SSLMode: inst.TargetSSLMode, Extra: params,
	}
	res := connection.Test(ctx, tgtParams)
	if !res.OK && inst.TargetCreateDB && isMissingDatabaseErr(res.Message) {
		if err := ensureTargetDatabase(ctx, inst); err != nil {
			res.Message = "auto-create failed: " + err.Error()
			okJSON(w, res)
			return
		}
		res = connection.Test(ctx, tgtParams)
	}
	okJSON(w, res)
}

// isMissingDatabaseErr returns true when the PG error message indicates the
// database itself doesn't exist (3D000), as opposed to a wrong password or
// network failure.
func isMissingDatabaseErr(msg string) bool {
	return strings.Contains(msg, "3D000") || strings.Contains(msg, "does not exist")
}

// ensureTargetDatabase creates inst.TargetDatabase via the PG default DB
// ("postgres") if it doesn't already exist. No-op when the DB is already there.
// Used both at instance creation (when target_create_db is checked) and on
// test_target_connection failures to avoid the chicken-and-egg dance.
func ensureTargetDatabase(ctx context.Context, inst *project.Instance) error {
	bootstrapCfg := connection.Params{
		Kind: "postgres", Host: inst.TargetHost, Port: inst.TargetPort,
		Database: "postgres", Username: inst.TargetUsername,
		Password: inst.TargetPassword, SSLMode: inst.TargetSSLMode,
	}
	pool, err := connection.OpenPostgres(ctx, bootstrapCfg)
	if err != nil {
		return fmt.Errorf("connect bootstrap DB: %w", err)
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, inst.TargetDatabase).Scan(&exists); err != nil {
		return fmt.Errorf("probe pg_database: %w", err)
	}
	if exists {
		return nil
	}
	// CREATE DATABASE doesn't accept bind parameters; quote-escape the name.
	quoted := `"` + strings.ReplaceAll(inst.TargetDatabase, `"`, `""`) + `"`
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}

func (d *Deps) rediscoverInstance(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "instanceID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid instanceID")
		return
	}
	inst, err := d.Repo.GetInstance(r.Context(), id)
	if errors.Is(err, project.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "instance not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	migrations, derr := d.discoverAndDraft(r.Context(), inst)
	resp := map[string]any{"migrations": migrations}
	if derr != nil {
		resp["discover_error"] = derr.Error()
	}
	okJSON(w, resp)
}

func (d *Deps) listInstanceMigrations(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "instanceID"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid instanceID")
		return
	}
	xs, err := d.Repo.ListMigrationsByInstance(r.Context(), id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	okJSON(w, map[string]any{"migrations": xs})
}

// ---- helpers ----

func validKind(k string) bool {
	switch k {
	case "mysql", "mariadb", "oracle", "oracle19", "db2", "db2zos":
		return true
	}
	return false
}

func (d *Deps) discoverAndDraft(ctx context.Context, inst *project.Instance) ([]project.Migration, error) {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := openSourceForInstance(dctx, inst)
	if err != nil {
		existing, _ := d.Repo.ListMigrationsByInstance(ctx, inst.ID)
		return existing, err
	}
	defer db.Close()

	schemas, err := discover.Schemas(dctx, db, inst.Kind)
	if err != nil {
		existing, _ := d.Repo.ListMigrationsByInstance(ctx, inst.ID)
		return existing, err
	}

	for _, sch := range schemas {
		targetDB, targetSchema := mapTarget(inst, sch)
		if _, err := d.Repo.CreateDraftMigration(ctx, inst.ID, sch, targetDB, targetSchema); err != nil {
			d.Log.Warn().Err(err).
				Str("instance", inst.ID.String()).
				Str("schema", sch).
				Msg("create draft migration")
		}
	}
	return d.Repo.ListMigrationsByInstance(ctx, inst.ID)
}

// mapTarget computes (target_db_name, target_schema_name) for a discovered
// source schema given the instance's strategy.
func mapTarget(inst *project.Instance, sourceSchema string) (string, string) {
	name := strings.ToLower(sourceSchema)
	switch inst.TargetStrategy {
	case "dedicated_db":
		// Each source schema → its own PG database, named after the schema,
		// schema=public. The instance's TargetDatabase is the admin endpoint
		// (typically "postgres") used to issue CREATE DATABASE.
		return name, "public"
	case "dedicated_schema":
		// All source schemas land in one PG database (TargetDatabase) under
		// a schema named after the source.
		return inst.TargetDatabase, name
	}
	return name, "public"
}

func openSourceForInstance(ctx context.Context, inst *project.Instance) (*sql.DB, error) {
	p := connection.Params{
		Kind: inst.Kind, Host: inst.Host, Port: inst.Port, Database: inst.Database,
		Username: inst.Username, Password: inst.Password, SSLMode: inst.SSLMode,
	}
	switch inst.Kind {
	case "mysql", "mariadb":
		return connection.OpenMySQL(ctx, p)
	case "oracle", "oracle19":
		return connection.OpenOracle(ctx, p)
	case "db2", "db2zos":
		return connection.OpenDB2(ctx, p)
	}
	return nil, errors.New("unsupported source kind: " + inst.Kind)
}
