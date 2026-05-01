// Package project contains the domain entities + repository for the wizard's
// top-level aggregates: projects, instances (source endpoints), connections
// (target PG admin) and migrations (one per source schema/database).
package project

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Project struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Instance bundles both the SOURCE super-user DSN (Oracle / MySQL / MariaDB
// endpoint, used for schema discovery + data extraction) and the TARGET PG
// super-user DSN where the migrated objects land. TargetStrategy decides how
// each discovered source schema is mapped on the PG side.
type Instance struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Name      string    `json:"name"`

	// Source DSN.
	Kind     string         `json:"kind"` // mysql|mariadb|oracle|oracle19
	Host     string         `json:"host"`
	Port     int            `json:"port"`
	Database string         `json:"database"`
	Username string         `json:"username"`
	Password string         `json:"-"`
	SSLMode  string         `json:"ssl_mode"`
	Params   map[string]any `json:"params,omitempty"`

	// Mapping strategy + target DSN.
	TargetStrategy string         `json:"target_strategy"` // dedicated_db|dedicated_schema
	TargetHost     string         `json:"target_host"`
	TargetPort     int            `json:"target_port"`
	TargetDatabase string         `json:"target_database"` // admin DB (dedicated_db) OR host DB (dedicated_schema)
	TargetUsername string         `json:"target_username"`
	TargetPassword string         `json:"-"`
	TargetSSLMode  string         `json:"target_ssl_mode"`
	TargetParams   map[string]any `json:"target_params,omitempty"`
	TargetCreateDB bool           `json:"target_create_db"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Migration is one source-schema → PG-cible mapping. It starts as a draft (no
// plan) when an instance is created, and becomes "planned" once translate +
// planner have run and the upsert has filled in the JSON columns.
type Migration struct {
	ID               uuid.UUID       `json:"id"`
	InstanceID       uuid.UUID       `json:"instance_id"`
	SourceSchemaName string          `json:"source_schema_name"`
	TargetDBName     string          `json:"target_db_name"`
	TargetSchemaName string          `json:"target_schema_name"`
	Status           string          `json:"status"` // draft|planned
	SourceSchema     json.RawMessage `json:"source_schema"`
	TargetPlan       json.RawMessage `json:"target_plan"`
	DDLScript        string          `json:"ddl_script"`
	DDLPostScript    string          `json:"ddl_post_script"`
	DataPlan         json.RawMessage `json:"data_plan"`
	TypeMappings     json.RawMessage `json:"type_mappings"`
	Explanations     json.RawMessage `json:"explanations"`
	Warnings         json.RawMessage `json:"warnings"`
	Prerequisites    json.RawMessage `json:"prerequisites"`
	AckedPrereqs     json.RawMessage `json:"acked_prereqs"`
	Options          json.RawMessage `json:"options"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`

	// LatestRunID / LatestRunStatus are denormalized pointers to the most
	// recent run of this migration, populated by ListMigrationsByInstance.
	// Empty when the migration was never launched.
	LatestRunID     *uuid.UUID `json:"latest_run_id,omitempty"`
	LatestRunStatus string     `json:"latest_run_status,omitempty"`
}

type Repo struct {
	Pool *pgxpool.Pool
}

func NewRepo(p *pgxpool.Pool) *Repo { return &Repo{Pool: p} }

// --- projects ---

func (r *Repo) CreateProject(ctx context.Context, p *Project) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return r.Pool.QueryRow(ctx, `
		INSERT INTO squishy.projects (id, name, slug, description)
		VALUES ($1,$2,$3,$4)
		RETURNING created_at, updated_at`,
		p.ID, p.Name, p.Slug, p.Description).Scan(&p.CreatedAt, &p.UpdatedAt)
}

func (r *Repo) GetProject(ctx context.Context, id uuid.UUID) (*Project, error) {
	var p Project
	err := r.Pool.QueryRow(ctx, `
		SELECT id, name, slug, description, created_at, updated_at
		  FROM squishy.projects WHERE id=$1`, id).
		Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

func (r *Repo) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := r.Pool.Query(ctx, `
		SELECT id, name, slug, description, created_at, updated_at
		  FROM squishy.projects ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *Repo) DeleteProject(ctx context.Context, id uuid.UUID) error {
	_, err := r.Pool.Exec(ctx, `DELETE FROM squishy.projects WHERE id=$1`, id)
	return err
}

// UpdateProject updates the mutable metadata (name, description) of a project.
// The slug is derived from the name at creation time and is intentionally
// immutable here to keep URLs stable.
func (r *Repo) UpdateProject(ctx context.Context, id uuid.UUID, name, description string) (*Project, error) {
	var p Project
	err := r.Pool.QueryRow(ctx, `
		UPDATE squishy.projects
		   SET name = $2, description = $3, updated_at = now()
		 WHERE id = $1
		 RETURNING id, name, slug, description, created_at, updated_at`,
		id, name, description).
		Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

// --- instances ---

func (r *Repo) CreateInstance(ctx context.Context, i *Instance) error {
	if i.ID == uuid.Nil {
		i.ID = uuid.New()
	}
	if i.SSLMode == "" {
		i.SSLMode = "disable"
	}
	if i.TargetSSLMode == "" {
		i.TargetSSLMode = "disable"
	}
	srcParams, _ := json.Marshal(i.Params)
	if len(srcParams) == 0 {
		srcParams = []byte("{}")
	}
	tgtParams, _ := json.Marshal(i.TargetParams)
	if len(tgtParams) == 0 {
		tgtParams = []byte("{}")
	}
	return r.Pool.QueryRow(ctx, `
		INSERT INTO squishy.instances
		  (id, project_id, name, kind, host, port, database, username, password, ssl_mode, params,
		   target_strategy,
		   target_host, target_port, target_database, target_username, target_password,
		   target_ssl_mode, target_params, target_create_db)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING created_at, updated_at`,
		i.ID, i.ProjectID, i.Name, i.Kind, i.Host, i.Port, i.Database,
		i.Username, i.Password, i.SSLMode, srcParams,
		i.TargetStrategy,
		i.TargetHost, i.TargetPort, i.TargetDatabase, i.TargetUsername, i.TargetPassword,
		i.TargetSSLMode, tgtParams, i.TargetCreateDB).
		Scan(&i.CreatedAt, &i.UpdatedAt)
}

func scanInstance(scan func(dest ...any) error, i *Instance) error {
	var srcParams, tgtParams []byte
	if err := scan(&i.ID, &i.ProjectID, &i.Name, &i.Kind, &i.Host, &i.Port, &i.Database,
		&i.Username, &i.Password, &i.SSLMode, &srcParams,
		&i.TargetStrategy,
		&i.TargetHost, &i.TargetPort, &i.TargetDatabase, &i.TargetUsername, &i.TargetPassword,
		&i.TargetSSLMode, &tgtParams, &i.TargetCreateDB,
		&i.CreatedAt, &i.UpdatedAt); err != nil {
		return err
	}
	if len(srcParams) > 0 {
		_ = json.Unmarshal(srcParams, &i.Params)
	}
	if len(tgtParams) > 0 {
		_ = json.Unmarshal(tgtParams, &i.TargetParams)
	}
	return nil
}

const instanceSelectCols = `id, project_id, name, kind, host, port, database, username, password, ssl_mode, params,
		       target_strategy,
		       target_host, target_port, target_database, target_username, target_password,
		       target_ssl_mode, target_params, target_create_db,
		       created_at, updated_at`

func (r *Repo) GetInstance(ctx context.Context, id uuid.UUID) (*Instance, error) {
	var i Instance
	row := r.Pool.QueryRow(ctx, `SELECT `+instanceSelectCols+` FROM squishy.instances WHERE id=$1`, id)
	if err := scanInstance(row.Scan, &i); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &i, nil
}

func (r *Repo) ListInstances(ctx context.Context, projectID uuid.UUID) ([]Instance, error) {
	rows, err := r.Pool.Query(ctx,
		`SELECT `+instanceSelectCols+` FROM squishy.instances WHERE project_id=$1 ORDER BY created_at ASC`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		var i Instance
		if err := scanInstance(rows.Scan, &i); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, nil
}

func (r *Repo) DeleteInstance(ctx context.Context, id uuid.UUID) error {
	_, err := r.Pool.Exec(ctx, `DELETE FROM squishy.instances WHERE id=$1`, id)
	return err
}

// UpdateInstance updates the mutable fields of an instance. The kind, project
// scope and creation timestamp are deliberately not editable: changing kind
// would invalidate already-planned migrations (parsed with the previous
// dialect) and re-parenting an instance is a copy-and-recreate operation.
//
// To preserve a stored target_password when the request omits it (i.e. the
// caller didn't include the secret in the JSON body), pass an empty string —
// the SQL keeps the existing value via COALESCE / NULLIF idiom.
func (r *Repo) UpdateInstance(ctx context.Context, i *Instance) error {
	if i.ID == uuid.Nil {
		return errors.New("update instance: id required")
	}
	if i.SSLMode == "" {
		i.SSLMode = "disable"
	}
	if i.TargetSSLMode == "" {
		i.TargetSSLMode = "disable"
	}
	srcParams, _ := json.Marshal(i.Params)
	if len(srcParams) == 0 {
		srcParams = []byte("{}")
	}
	tgtParams, _ := json.Marshal(i.TargetParams)
	if len(tgtParams) == 0 {
		tgtParams = []byte("{}")
	}
	tag, err := r.Pool.Exec(ctx, `
		UPDATE squishy.instances SET
		  name = $2,
		  host = $3, port = $4, database = $5, username = $6,
		  password = CASE WHEN $7 = '' THEN password ELSE $7 END,
		  ssl_mode = $8, params = $9,
		  target_strategy = $10,
		  target_host = $11, target_port = $12, target_database = $13, target_username = $14,
		  target_password = CASE WHEN $15 = '' THEN target_password ELSE $15 END,
		  target_ssl_mode = $16, target_params = $17, target_create_db = $18,
		  updated_at = now()
		 WHERE id = $1`,
		i.ID, i.Name,
		i.Host, i.Port, i.Database, i.Username, i.Password, i.SSLMode, srcParams,
		i.TargetStrategy,
		i.TargetHost, i.TargetPort, i.TargetDatabase, i.TargetUsername, i.TargetPassword,
		i.TargetSSLMode, tgtParams, i.TargetCreateDB)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- migrations ---

// CreateDraftMigration inserts a draft migration row for (instanceID, sourceSchemaName)
// if it doesn't already exist. Used by instance creation/rediscover to materialize
// one row per discovered source schema. Idempotent: existing rows are left untouched.
func (r *Repo) CreateDraftMigration(ctx context.Context, instanceID uuid.UUID,
	sourceSchema, targetDB, targetSchema string) (*Migration, error) {
	if sourceSchema == "" || targetDB == "" || targetSchema == "" {
		return nil, errors.New("source_schema_name / target_db_name / target_schema_name required")
	}
	id := uuid.New()
	var m Migration
	err := r.Pool.QueryRow(ctx, `
		INSERT INTO squishy.migrations
		  (id, instance_id, source_schema_name, target_db_name, target_schema_name, status)
		VALUES ($1,$2,$3,$4,$5,'draft')
		ON CONFLICT (instance_id, source_schema_name) DO UPDATE SET
		  -- on rediscover we keep the existing row but refresh target naming
		  -- in case the strategy changed (rare, but cheap to support).
		  target_db_name=EXCLUDED.target_db_name,
		  target_schema_name=EXCLUDED.target_schema_name,
		  updated_at=now()
		RETURNING id, instance_id, source_schema_name, target_db_name, target_schema_name,
		          status, source_schema, target_plan, ddl_script, ddl_post_script,
		          data_plan, type_mappings, explanations, warnings, prerequisites, acked_prereqs,
		          options, created_at, updated_at`,
		id, instanceID, sourceSchema, targetDB, targetSchema).
		Scan(&m.ID, &m.InstanceID, &m.SourceSchemaName, &m.TargetDBName, &m.TargetSchemaName,
			&m.Status, &m.SourceSchema, &m.TargetPlan, &m.DDLScript, &m.DDLPostScript,
			&m.DataPlan, &m.TypeMappings, &m.Explanations, &m.Warnings, &m.Prerequisites,
			&m.AckedPrereqs, &m.Options, &m.CreatedAt, &m.UpdatedAt)
	return &m, err
}

// UpsertPlannedMigration replaces (or inserts) the full plan payload for an
// existing draft migration. Acked prerequisites are preserved on update — a
// re-plan that introduces new blocking prereqs forces the user to ack them
// again, but acks for prereqs that survive are kept.
func (r *Repo) UpsertPlannedMigration(ctx context.Context, m *Migration) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	return r.Pool.QueryRow(ctx, `
		INSERT INTO squishy.migrations
		  (id, instance_id, source_schema_name, target_db_name, target_schema_name, status,
		   source_schema, target_plan, ddl_script, ddl_post_script,
		   data_plan, type_mappings, explanations, warnings, prerequisites, options)
		VALUES ($1,$2,$3,$4,$5,'planned',$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (instance_id, source_schema_name) DO UPDATE SET
		  target_db_name=EXCLUDED.target_db_name,
		  target_schema_name=EXCLUDED.target_schema_name,
		  status='planned',
		  source_schema=EXCLUDED.source_schema,
		  target_plan=EXCLUDED.target_plan,
		  ddl_script=EXCLUDED.ddl_script,
		  ddl_post_script=EXCLUDED.ddl_post_script,
		  data_plan=EXCLUDED.data_plan,
		  type_mappings=EXCLUDED.type_mappings,
		  explanations=EXCLUDED.explanations,
		  warnings=EXCLUDED.warnings,
		  prerequisites=EXCLUDED.prerequisites,
		  options=EXCLUDED.options,
		  updated_at=now()
		RETURNING id, status, created_at, updated_at`,
		m.ID, m.InstanceID, m.SourceSchemaName, m.TargetDBName, m.TargetSchemaName,
		jsonOrEmpty(m.SourceSchema),
		jsonOrEmpty(m.TargetPlan),
		m.DDLScript, m.DDLPostScript,
		jsonOrEmpty(m.DataPlan),
		jsonOrEmpty(m.TypeMappings),
		jsonOrEmpty(m.Explanations),
		jsonOrEmpty(m.Warnings),
		jsonArrayOrEmpty(m.Prerequisites),
		jsonOrEmpty(m.Options)).
		Scan(&m.ID, &m.Status, &m.CreatedAt, &m.UpdatedAt)
}

func (r *Repo) GetMigration(ctx context.Context, id uuid.UUID) (*Migration, error) {
	var m Migration
	err := r.Pool.QueryRow(ctx, `
		SELECT id, instance_id, source_schema_name, target_db_name, target_schema_name, status,
		       source_schema, target_plan, ddl_script, ddl_post_script,
		       data_plan, type_mappings, explanations, warnings, prerequisites, acked_prereqs,
		       options, created_at, updated_at
		  FROM squishy.migrations WHERE id=$1`, id).
		Scan(&m.ID, &m.InstanceID, &m.SourceSchemaName, &m.TargetDBName, &m.TargetSchemaName,
			&m.Status, &m.SourceSchema, &m.TargetPlan, &m.DDLScript, &m.DDLPostScript,
			&m.DataPlan, &m.TypeMappings, &m.Explanations, &m.Warnings, &m.Prerequisites,
			&m.AckedPrereqs, &m.Options, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}

func (r *Repo) ListMigrationsByInstance(ctx context.Context, instanceID uuid.UUID) ([]Migration, error) {
	// Joining a LATERAL pick of the most recent run lets us surface both the
	// migration's plan-status and its execution-status in one query, which is
	// what the UI's instance table needs to render meaningful badges.
	rows, err := r.Pool.Query(ctx, `
		SELECT m.id, m.instance_id, m.source_schema_name, m.target_db_name, m.target_schema_name,
		       m.status, m.ddl_script, m.ddl_post_script, m.prerequisites, m.acked_prereqs,
		       m.created_at, m.updated_at,
		       lr.id, lr.status::text
		  FROM squishy.migrations m
		  LEFT JOIN LATERAL (
		    SELECT id, status
		      FROM squishy.runs r
		     WHERE r.migration_id = m.id
		     ORDER BY r.created_at DESC
		     LIMIT 1
		  ) lr ON TRUE
		 WHERE m.instance_id = $1
		 ORDER BY m.source_schema_name ASC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Migration
	for rows.Next() {
		var m Migration
		var lrID *uuid.UUID
		var lrStatus *string
		if err := rows.Scan(&m.ID, &m.InstanceID, &m.SourceSchemaName, &m.TargetDBName, &m.TargetSchemaName,
			&m.Status, &m.DDLScript, &m.DDLPostScript, &m.Prerequisites, &m.AckedPrereqs,
			&m.CreatedAt, &m.UpdatedAt, &lrID, &lrStatus); err != nil {
			return nil, err
		}
		m.LatestRunID = lrID
		if lrStatus != nil {
			m.LatestRunStatus = *lrStatus
		}
		out = append(out, m)
	}
	return out, nil
}

// SetAckedPrereqs replaces the set of acknowledged prerequisite IDs on a
// migration. The list is the source of truth consulted before a run can
// start (see httpapi.startRun).
func (r *Repo) SetAckedPrereqs(ctx context.Context, migrationID uuid.UUID, ids []string) error {
	payload, _ := json.Marshal(ids)
	_, err := r.Pool.Exec(ctx,
		`UPDATE squishy.migrations SET acked_prereqs=$2 WHERE id=$1`,
		migrationID, payload)
	return err
}

// ErrNotFound is the canonical not-found error for repository calls.
var ErrNotFound = errors.New("not found")

func jsonOrEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

func jsonArrayOrEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	return b
}
