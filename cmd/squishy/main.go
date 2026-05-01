package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"gitlab.com/dalibo/squishy/internal/config"
	"gitlab.com/dalibo/squishy/internal/connection"
	"gitlab.com/dalibo/squishy/internal/dataxfer"
	"gitlab.com/dalibo/squishy/internal/events"
	"gitlab.com/dalibo/squishy/internal/httpapi"
	"gitlab.com/dalibo/squishy/internal/project"
	"gitlab.com/dalibo/squishy/internal/queue"
	"gitlab.com/dalibo/squishy/internal/storage"
	"gitlab.com/dalibo/squishy/internal/version"
	"gitlab.com/dalibo/squishy/internal/worker"
)

func main() {
	log := zerolog.New(os.Stdout).With().Timestamp().Str("svc", "squishy").Logger()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("config")
	}
	setLogLevel(cfg.LogLevel)
	log.Info().Str("version", version.Version).Int("workers", cfg.Workers).Msg("starting")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := storage.Open(ctx, cfg.PGDSN)
	if err != nil {
		log.Fatal().Err(err).Msg("pg open")
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		log.Fatal().Err(err).Msg("migrate")
	}

	bus := events.NewBus(db.Pool)
	repo := project.NewRepo(db.Pool)

	depsWorker := &worker.Deps{
		AppDB:     db.Pool,
		Bus:       bus,
		BatchSize: cfg.BatchSize,
	}

	// Lazy connection resolver. For each job we walk run → migration → instance
	// → project to materialize three pools:
	//   - source: instance's super-user DSN, scoped to migration.source_schema_name
	//   - target: project's PG admin endpoint, database overridden to migration.target_db_name
	//   - admin : project's PG admin endpoint, no database override (for CREATE DATABASE)
	// The admin pool is shared across all migrations of a given project.
	lazy := &connCache{appDB: db.Pool, log: log}

	kinds := []string{
		"inspect", "create_target_db", "create_ddl", "copy_table", "copy_batch",
		"create_index", "create_fk", "create_routine", "validate",
	}
	handlers := worker.HandlerRegistry{}
	for _, k := range kinds {
		k := k
		handlers[k] = func(ctx context.Context, j *queue.Job) error {
			mc, err := lazy.forRun(ctx, j.RunID)
			if err != nil {
				return err
			}
			// `create_target_db` runs before its target database exists, so
			// opening the target pool would fail. Only the admin pool is
			// needed there. Every other handler needs the target pool wired.
			var tgt *pgxpool.Pool
			if k != "create_target_db" {
				tgt, err = mc.target(ctx)
				if err != nil {
					return err
				}
			}
			scoped := &worker.Deps{
				AppDB:         depsWorker.AppDB,
				SourceDB:      mc.src,
				SourceDialect: mc.dia,
				TargetPool:    tgt,
				AdminPool:     mc.admin,
				Bus:           depsWorker.Bus,
				BatchSize:     depsWorker.BatchSize,
			}
			h, ok := scoped.Handlers()[k]
			if !ok {
				return fmt.Errorf("no handler for kind %q", k)
			}
			return h(ctx, j)
		}
	}

	pool := &worker.Pool{
		Store:    queue.New(db.Pool),
		Bus:      bus,
		Handlers: handlers,
		Workers:  cfg.Workers,
		LockedBy: cfg.WorkerID,
		Log:      log,
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := pool.Run(ctx); err != nil {
			log.Error().Err(err).Msg("worker pool")
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		dispatchLoop(ctx, db.Pool, log)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		runStateLoop(ctx, db.Pool, bus, log)
	}()

	h := httpapi.Handler(httpapi.Deps{DB: db.Pool, Repo: repo, Bus: bus, Log: log})
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Str("addr", cfg.HTTPAddr).Msg("listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("http")
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutdown")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
	wg.Wait()
}

func setLogLevel(lvl string) {
	switch lvl {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
}

// dispatchLoop unlocks dependent steps once all parents succeed.
func dispatchLoop(ctx context.Context, pool *pgxpool.Pool, log zerolog.Logger) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	q := queue.New(pool)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rows, err := pool.Query(ctx, `
			SELECT s.id, s.run_id, s.kind, s.payload, s.priority
			  FROM squishy.steps s
			 WHERE s.status='pending'
			   AND s.unlocked = true
			   AND NOT EXISTS (
			     SELECT 1 FROM unnest(s.depends_on) dep
			      WHERE NOT EXISTS (
			        SELECT 1 FROM squishy.steps p WHERE p.id=dep AND p.status='succeeded'
			      )
			   )
			   AND NOT EXISTS (
			     SELECT 1 FROM squishy.jobs j
			      WHERE j.step_id = s.id AND j.batch_id IS NULL
			   )`)
		if err != nil {
			log.Warn().Err(err).Msg("dispatch query")
			continue
		}
		type ready struct {
			id       uuid.UUID
			runID    uuid.UUID
			kind     string
			payload  []byte
			priority int16
		}
		var batch []ready
		for rows.Next() {
			var r ready
			if err := rows.Scan(&r.id, &r.runID, &r.kind, &r.payload, &r.priority); err != nil {
				continue
			}
			batch = append(batch, r)
		}
		rows.Close()
		for _, r := range batch {
			_, err := q.Enqueue(ctx, nil, queue.Job{
				RunID: r.runID, StepID: r.id,
				Kind: r.kind, Payload: r.payload, Priority: r.priority,
			})
			if err != nil {
				log.Warn().Err(err).Msg("dispatch enqueue")
			}
		}
	}
}

// runStateLoop flips runs.status once all steps terminate.
//
// A run is `succeeded` when every step is succeeded.
//
// A run is `failed` as soon as no further progress is possible. That
// covers two situations:
//
//  1. Every step is in a terminal state (succeeded/failed/cancelled),
//     and at least one is failed/cancelled.
//  2. Some steps are still `pending`, but every one of them transitively
//     depends on a failed/cancelled step — the dispatcher will never
//     queue them, so the run is permanently stuck. We detect this by
//     checking that no `pending` step is currently runnable (= has all
//     its direct dependencies satisfied) AND no step is `running` AND
//     at least one is failed.
//
// Without case (2) the run sits at `running` forever after a level-1
// failure (e.g. create_ddl), because the bulk of the DAG is `pending`
// behind it.
func runStateLoop(ctx context.Context, pool *pgxpool.Pool, bus *events.Bus, log zerolog.Logger) {
	_ = log
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rows, err := pool.Query(ctx, `SELECT id FROM squishy.runs WHERE status='running'`)
		if err != nil {
			continue
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			var total, done, fail, running int64
			var hasRunnable bool
			err := pool.QueryRow(ctx, `
				SELECT count(*),
				       count(*) FILTER (WHERE status='succeeded'),
				       count(*) FILTER (WHERE status IN ('failed','cancelled')),
				       count(*) FILTER (WHERE status='running'),
				       EXISTS (
				         SELECT 1 FROM squishy.steps p
				          WHERE p.run_id=$1 AND p.status='pending'
				            AND NOT EXISTS (
				                  SELECT 1 FROM squishy.steps d
				                   WHERE d.run_id=$1
				                     AND d.id = ANY (p.depends_on)
				                     AND d.status <> 'succeeded'
				                )
				       )
				  FROM squishy.steps WHERE run_id=$1`, id).Scan(&total, &done, &fail, &running, &hasRunnable)
			if err != nil || total == 0 {
				continue
			}
			var newStatus string
			switch {
			case done >= total:
				newStatus = "succeeded"
			case fail > 0 && running == 0 && !hasRunnable:
				// No step is running, none can be queued (every
				// pending step waits on a failed dep), and we already
				// have at least one failure → the run is stuck.
				newStatus = "failed"
			default:
				continue
			}
			if _, err := pool.Exec(ctx, `
				UPDATE squishy.runs SET status=$1, finished_at=now() WHERE id=$2`, newStatus, id); err == nil {
				bus.Publish(ctx, events.Event{
					RunID: id, Kind: "run.status", Level: "info",
					Message: "run " + newStatus,
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Lazy per-migration connection resolver.
// ---------------------------------------------------------------------------

type migConns struct {
	src       *sql.DB
	dia       dataxfer.SourceDialect
	admin     *pgxpool.Pool
	targetCfg connection.Params

	tgtMu sync.Mutex
	tgt   *pgxpool.Pool
}

// target opens (or returns the cached) target PG pool for the per-migration
// database. Lazy because for the dedicated_db strategy the database does not
// exist until the create_target_db step has run; opening at resolve time would
// fail with "database does not exist".
func (m *migConns) target(ctx context.Context) (*pgxpool.Pool, error) {
	m.tgtMu.Lock()
	defer m.tgtMu.Unlock()
	if m.tgt != nil {
		return m.tgt, nil
	}
	p, err := connection.OpenPostgres(ctx, m.targetCfg)
	if err != nil {
		return nil, err
	}
	m.tgt = p
	return p, nil
}

type connCache struct {
	mu        sync.Mutex
	appDB     *pgxpool.Pool
	log       zerolog.Logger
	byMig     map[uuid.UUID]*migConns     // migrationID → pools
	byProject map[uuid.UUID]*pgxpool.Pool // projectID → admin pool (shared)
}

// forRun resolves the per-migration pools for the given run, opening + caching
// them on first access.
func (c *connCache) forRun(ctx context.Context, runID uuid.UUID) (*migConns, error) {
	var migID, instID uuid.UUID
	var srcKind, srcHost, srcDB, srcUser, srcPass, srcSSL string
	var srcPort int
	var srcParamsRaw []byte
	var sourceSchema, targetDBName string
	var tHost, tAdminDB, tUser, tPass, tSSL string
	var tPort int
	err := c.appDB.QueryRow(ctx, `
		SELECT m.id, m.instance_id,
		       i.kind::text, i.host, i.port, i.database, i.username, i.password, i.ssl_mode, i.params,
		       m.source_schema_name, m.target_db_name,
		       i.target_host, i.target_port, i.target_database, i.target_username, i.target_password,
		       i.target_ssl_mode
		  FROM squishy.runs r
		  JOIN squishy.migrations m ON m.id = r.migration_id
		  JOIN squishy.instances  i ON i.id = m.instance_id
		 WHERE r.id = $1`, runID).
		Scan(&migID, &instID, &srcKind, &srcHost, &srcPort,
			&srcDB, &srcUser, &srcPass, &srcSSL, &srcParamsRaw,
			&sourceSchema, &targetDBName,
			&tHost, &tPort, &tAdminDB, &tUser, &tPass, &tSSL)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if mc, ok := c.byMig[migID]; ok {
		c.mu.Unlock()
		return mc, nil
	}
	c.mu.Unlock()

	srcExtras := map[string]string{}
	if len(srcParamsRaw) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(srcParamsRaw, &raw); err == nil {
			for k, v := range raw {
				if s, ok := v.(string); ok {
					srcExtras[k] = s
				}
			}
		}
	}

	// MySQL/MariaDB: scope the source pool to the migration's source database.
	// Oracle: USER/CURRENT_SCHEMA is set per-query by the introspector/copy
	// code, so the pool stays at the service-name level.
	// DB2: pool stays at the database level; SET CURRENT SCHEMA is issued
	// per-session via Extra["CurrentSchema"] so go_ibm_db propagates it on
	// every check-out.
	srcDatabase := srcDB
	if srcKind == "mysql" || srcKind == "mariadb" {
		srcDatabase = sourceSchema
	}
	if srcKind == "db2" || srcKind == "db2zos" {
		if srcExtras == nil {
			srcExtras = map[string]string{}
		}
		// DB2 CLI keyword ensuring CURRENT SCHEMA is set on every connection
		// the driver pool checks out (otherwise go_ibm_db hands you a fresh
		// session with the user's default schema).
		if _, ok := srcExtras["CurrentSchema"]; !ok && sourceSchema != "" {
			srcExtras["CurrentSchema"] = strings.ToUpper(sourceSchema)
		}
	}
	srcParams := connection.Params{
		Kind: srcKind, Host: srcHost, Port: srcPort, Database: srcDatabase,
		Username: srcUser, Password: srcPass, SSLMode: srcSSL, Extra: srcExtras,
	}
	var src *sql.DB
	var dia dataxfer.SourceDialect
	switch srcKind {
	case "mysql", "mariadb":
		src, err = connection.OpenMySQL(ctx, srcParams)
		dia = dataxfer.MySQLSource()
	case "oracle", "oracle19":
		src, err = connection.OpenOracle(ctx, srcParams)
		dia = dataxfer.OracleSource()
	case "db2":
		src, err = connection.OpenDB2(ctx, srcParams)
		dia = dataxfer.DB2Source()
	case "db2zos":
		src, err = connection.OpenDB2(ctx, srcParams)
		dia = dataxfer.DB2zOSSource()
	default:
		return nil, fmt.Errorf("unsupported source kind: %s", srcKind)
	}
	if err != nil {
		return nil, err
	}

	adminCfg := connection.Params{
		Kind: "postgres", Host: tHost, Port: tPort, Database: tAdminDB,
		Username: tUser, Password: tPass, SSLMode: tSSL,
	}
	// admin pool is shared across all migrations of a given INSTANCE (since
	// the target endpoint is now per-instance, not per-project).
	c.mu.Lock()
	admin, ok := c.byProject[instID]
	c.mu.Unlock()
	if !ok {
		admin, err = connection.OpenPostgres(ctx, adminCfg)
		if err != nil {
			src.Close()
			return nil, err
		}
		c.mu.Lock()
		if c.byProject == nil {
			c.byProject = map[uuid.UUID]*pgxpool.Pool{}
		}
		if existing, ok := c.byProject[instID]; ok {
			admin.Close()
			admin = existing
		} else {
			c.byProject[instID] = admin
		}
		c.mu.Unlock()
	}

	targetCfg := adminCfg
	targetCfg.Database = targetDBName

	mc := &migConns{src: src, dia: dia, admin: admin, targetCfg: targetCfg}

	c.mu.Lock()
	if c.byMig == nil {
		c.byMig = map[uuid.UUID]*migConns{}
	}
	c.byMig[migID] = mc
	c.mu.Unlock()
	return mc, nil
}
