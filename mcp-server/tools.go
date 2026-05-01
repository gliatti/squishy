package main

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerTools wires every MCP tool onto the server. Each tool is a thin
// wrapper on a squishy /api/v1 route. The model is:
//   project (1) ──< instance (N: source super-user endpoint) ──< migration (N: per source schema)
//   project (1) ──── target connection (1: PG super-user with CREATEDB)
func registerTools(s *server.MCPServer) {
	// ---- projects ----
	s.AddTool(mcp.NewTool("list_projects",
		mcp.WithDescription("List all squishy migration projects."),
	), listProjectsHandler)

	s.AddTool(mcp.NewTool("get_project",
		mcp.WithDescription("Get a single squishy project by its UUID."),
		mcp.WithString("project_id", mcp.Required(), mcp.Description("Project UUID")),
	), getProjectHandler)

	s.AddTool(mcp.NewTool("create_project",
		mcp.WithDescription("Create a new migration project. The slug is auto-derived from the name."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Human-readable project name")),
		mcp.WithString("description", mcp.Description("Optional free-text description")),
	), createProjectHandler)

	s.AddTool(mcp.NewTool("delete_project",
		mcp.WithDescription("Delete a project, all its instances and the associated migrations/runs."),
		mcp.WithString("project_id", mcp.Required(), mcp.Description("Project UUID")),
	), deleteProjectHandler)

	s.AddTool(mcp.NewTool("update_project",
		mcp.WithDescription("Update the metadata (name, description) of a project. The slug is derived from the name at creation time and is immutable."),
		mcp.WithString("project_id", mcp.Required(), mcp.Description("Project UUID")),
		mcp.WithString("name", mcp.Required(), mcp.Description("New display name")),
		mcp.WithString("description", mcp.Description("New free-text description")),
	), updateProjectHandler)

	// ---- instances (source + target endpoints, mapping strategy) ----
	s.AddTool(mcp.NewTool("list_instances",
		mcp.WithDescription("List the source instances configured for a project."),
		mcp.WithString("project_id", mcp.Required(), mcp.Description("Project UUID")),
	), listInstancesHandler)

	s.AddTool(mcp.NewTool("create_instance",
		mcp.WithDescription("Create an instance for a project: bundles the SOURCE super-user DSN of an Oracle/MySQL/MariaDB endpoint with the TARGET PG super-user DSN where migrated objects land, plus a mapping strategy. Connects to the source, enumerates non-system schemas/databases, and inserts one draft migration per discovered schema. Returns the new instance and its draft migrations."),
		mcp.WithString("project_id", mcp.Required(), mcp.Description("Project UUID")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Display name for the instance (e.g. 'oracle-prod')")),

		mcp.WithString("kind", mcp.Required(), mcp.Description("Source DB kind"), mcp.Enum("mysql", "mariadb", "oracle", "oracle19", "db2", "db2zos")),
		mcp.WithString("host", mcp.Required(), mcp.Description("Source hostname or IP")),
		mcp.WithNumber("port", mcp.Required(), mcp.Description("Source TCP port")),
		mcp.WithString("database", mcp.Required(), mcp.Description("Default database (MySQL/MariaDB) or service/SID (Oracle) — used for the initial source connection; the per-migration source schema is selected later")),
		mcp.WithString("username", mcp.Required(), mcp.Description("Source super-user login (Oracle SYSTEM, MySQL root, …)")),
		mcp.WithString("password", mcp.Description("Source password")),
		mcp.WithString("ssl_mode", mcp.Description("Source SSL mode (driver-specific, default 'disable')")),

		mcp.WithString("target_strategy", mcp.Required(), mcp.Description("How each discovered source schema maps to PG: dedicated_db = one PG database per source schema (auto CREATE DATABASE; target_database is the admin DB); dedicated_schema = one PG schema per source schema, all in target_database"), mcp.Enum("dedicated_db", "dedicated_schema")),
		mcp.WithString("target_host", mcp.Required(), mcp.Description("Target PG hostname or IP")),
		mcp.WithNumber("target_port", mcp.Required(), mcp.Description("Target PG port (typically 5432)")),
		mcp.WithString("target_database", mcp.Required(), mcp.Description("For dedicated_db: admin DB used to issue CREATE DATABASE (typically 'postgres'). For dedicated_schema: host DB for all migrated schemas")),
		mcp.WithString("target_username", mcp.Required(), mcp.Description("Target PG super-user login. Must hold CREATEDB if target_strategy=dedicated_db")),
		mcp.WithString("target_password", mcp.Description("Target PG password")),
		mcp.WithString("target_ssl_mode", mcp.Description("Target PG SSL mode (driver-specific, default 'disable')")),
		mcp.WithBoolean("target_create_db", mcp.Description("If true, squishy will CREATE DATABASE target_database via the PG default DB ('postgres') if it doesn't exist. Useful for dev / CI; the user must hold CREATEDB.")),
	), createInstanceHandler)

	s.AddTool(mcp.NewTool("get_instance",
		mcp.WithDescription("Get one instance and its list of migrations."),
		mcp.WithString("instance_id", mcp.Required(), mcp.Description("Instance UUID")),
	), getInstanceHandler)

	s.AddTool(mcp.NewTool("delete_instance",
		mcp.WithDescription("Delete an instance and all its migrations/runs."),
		mcp.WithString("instance_id", mcp.Required(), mcp.Description("Instance UUID")),
	), deleteInstanceHandler)

	s.AddTool(mcp.NewTool("update_instance",
		mcp.WithDescription("Update an instance's source/target DSN, mapping strategy and target_create_db. Project scope and source kind are immutable. Pass empty 'password' / 'target_password' to keep the stored secret untouched."),
		mcp.WithString("instance_id", mcp.Required(), mcp.Description("Instance UUID")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Display name")),

		mcp.WithString("host", mcp.Required(), mcp.Description("Source hostname or IP")),
		mcp.WithNumber("port", mcp.Required(), mcp.Description("Source TCP port")),
		mcp.WithString("database", mcp.Required(), mcp.Description("Source default DB / service name")),
		mcp.WithString("username", mcp.Required(), mcp.Description("Source super-user login")),
		mcp.WithString("password", mcp.Description("Source password — leave empty to keep the stored value")),
		mcp.WithString("ssl_mode", mcp.Description("Source SSL mode (default 'disable')")),

		mcp.WithString("target_strategy", mcp.Required(), mcp.Description("dedicated_db | dedicated_schema"), mcp.Enum("dedicated_db", "dedicated_schema")),
		mcp.WithString("target_host", mcp.Required(), mcp.Description("Target PG hostname")),
		mcp.WithNumber("target_port", mcp.Required(), mcp.Description("Target PG port")),
		mcp.WithString("target_database", mcp.Required(), mcp.Description("Target PG admin or host DB")),
		mcp.WithString("target_username", mcp.Required(), mcp.Description("Target PG super-user")),
		mcp.WithString("target_password", mcp.Description("Target password — leave empty to keep the stored value")),
		mcp.WithString("target_ssl_mode", mcp.Description("Target SSL mode (default 'disable')")),
		mcp.WithBoolean("target_create_db", mcp.Description("Auto-create target_database via PG default DB if missing")),
	), updateInstanceHandler)

	s.AddTool(mcp.NewTool("test_instance_connection",
		mcp.WithDescription("Probe an instance's SOURCE super-user connection."),
		mcp.WithString("instance_id", mcp.Required(), mcp.Description("Instance UUID")),
	), testInstanceConnectionHandler)

	s.AddTool(mcp.NewTool("test_instance_target_connection",
		mcp.WithDescription("Probe an instance's TARGET PG super-user connection."),
		mcp.WithString("instance_id", mcp.Required(), mcp.Description("Instance UUID")),
	), testInstanceTargetConnectionHandler)

	s.AddTool(mcp.NewTool("rediscover_instance",
		mcp.WithDescription("Re-enumerate the source schemas of an instance and create draft migrations for any new ones. Existing migrations are kept untouched (their plans survive)."),
		mcp.WithString("instance_id", mcp.Required(), mcp.Description("Instance UUID")),
	), rediscoverInstanceHandler)

	s.AddTool(mcp.NewTool("list_migrations",
		mcp.WithDescription("List the migrations of an instance (one per discovered source schema)."),
		mcp.WithString("instance_id", mcp.Required(), mcp.Description("Instance UUID")),
	), listMigrationsHandler)

	// ---- migrations ----
	s.AddTool(mcp.NewTool("get_migration",
		mcp.WithDescription("Get a migration's full payload (source schema, target plan, DDL scripts, type mappings, warnings, prerequisites)."),
		mcp.WithString("migration_id", mcp.Required(), mcp.Description("Migration UUID")),
	), getMigrationHandler)

	s.AddTool(mcp.NewTool("inspect_migration",
		mcp.WithDescription("Introspect the source schema of a migration: extract the full schema (tables, views, routines, triggers, events). Uses the parent instance's super-user connection scoped to migration.source_schema_name."),
		mcp.WithString("migration_id", mcp.Required(), mcp.Description("Migration UUID")),
	), inspectMigrationHandler)

	s.AddTool(mcp.NewTool("plan_migration",
		mcp.WithDescription("Run the dialect parser on the inspected source schema, translate it to PostgreSQL using the migration's target_db_name/target_schema_name, and upsert the migration row with the freshly-computed plan. Default response is a COMPACT summary: counts, head of warnings, prerequisites, head of ddl_script, aggregated explanation/type-mapping summaries. Section + filters let you drill in without dumping the full plan (which can exceed 16 MB on large schemas)."),
		mcp.WithString("migration_id", mcp.Required(), mcp.Description("Migration UUID")),
		mcp.WithString("section", mcp.Description("Which slice to return: 'summary' (default), 'ddl', 'ddl_post', 'warnings', 'explanations', 'type_mappings'"), mcp.Enum("summary", "ddl", "ddl_post", "warnings", "explanations", "type_mappings")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset. For section=ddl|ddl_post it counts characters; for warnings|explanations|type_mappings it counts elements.")),
		mcp.WithNumber("limit", mcp.Description("Pagination limit. Defaults: 8000 chars for ddl/ddl_post, 200 elements for warnings/explanations/type_mappings.")),
		mcp.WithString("severity", mcp.Description("Filter warnings by severity (e.g. 'blocking', 'info'). Applies to summary + section=warnings.")),
		mcp.WithString("kind", mcp.Description("Filter warnings by kind. Applies to summary + section=warnings.")),
		mcp.WithString("object", mcp.Description("Substring filter on the .object field of warnings AND explanations.")),
		mcp.WithString("level", mcp.Description("Filter explanations by level ('warn' | 'info').")),
		mcp.WithString("reason_contains", mcp.Description("Substring filter on the .reason field of explanations.")),
	), planMigrationHandler)

	s.AddTool(mcp.NewTool("get_prerequisites",
		mcp.WithDescription("Get the list of prerequisites for a migration and their current acknowledgement state."),
		mcp.WithString("migration_id", mcp.Required(), mcp.Description("Migration UUID")),
	), getPrerequisitesHandler)

	s.AddTool(mcp.NewTool("ack_prerequisites",
		mcp.WithDescription("Replace the acknowledged-prerequisites set for a migration. All blocking prereqs must be acknowledged before start_run can succeed."),
		mcp.WithString("migration_id", mcp.Required(), mcp.Description("Migration UUID")),
		mcp.WithArray("acked", mcp.Required(), mcp.Description("List of prerequisite IDs to mark as acknowledged"),
			mcp.Items(map[string]any{"type": "string"})),
	), ackPrerequisitesHandler)

	s.AddTool(mcp.NewTool("start_run",
		mcp.WithDescription("Start a new migration run. Expands the data plan into steps + jobs. For instances with target_strategy=dedicated_db a leading create_target_db step issues CREATE DATABASE before anything else runs. Fails with 409 if a run is already active for this migration or if blocking prereqs are unresolved. Use skip_data=true to omit copy_table + create_index steps when iterating on routine/trigger translation after a successful first run."),
		mcp.WithString("migration_id", mcp.Required(), mcp.Description("Migration UUID")),
		mcp.WithString("mode", mcp.Description("Dispatch mode — 'auto' runs the whole DAG, 'manual' requires play_level/play_step"), mcp.Enum("auto", "manual")),
		mcp.WithBoolean("skip_data", mcp.Description("Skip copy_table + create_index steps. Use for fast trigger/routine iteration when PG already holds the data from a previous successful run.")),
	), startRunHandler)

	// ---- runs ----
	s.AddTool(mcp.NewTool("get_run",
		mcp.WithDescription("Get a run's progress snapshot: status, totals, rows done/total."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
	), getRunHandler)

	s.AddTool(mcp.NewTool("list_steps",
		mcp.WithDescription("List steps of a run with their status, attempts, errors, row counts and DAG info (depends_on, level). Optional filters narrow the result set — typical usage: status='failed' to surface only failing steps with their last_error. Pagination via offset/limit (default limit=100, max=1000). Response always includes total + next_offset."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
		mcp.WithString("status", mcp.Description("Optional: only return steps with this status"), mcp.Enum("pending", "running", "succeeded", "failed", "cancelled")),
		mcp.WithString("kind", mcp.Description("Optional: only return steps of this kind (e.g. create_target_db, create_routine, copy_table, create_index, create_fk, validate)")),
		mcp.WithNumber("offset", mcp.Description("Pagination offset (default 0)")),
		mcp.WithNumber("limit", mcp.Description("Page size (default 100, max 1000)")),
	), listStepsHandler)

	s.AddTool(mcp.NewTool("list_batches",
		mcp.WithDescription("List all batches of a copy_table step (PK ranges and their copy status)."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
		mcp.WithString("step_id", mcp.Required(), mcp.Description("Step UUID")),
	), listBatchesHandler)

	s.AddTool(mcp.NewTool("play_step",
		mcp.WithDescription("Unlock a single step and reset its failed/cancelled status so the dispatcher picks it up (manual mode)."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
		mcp.WithString("step_id", mcp.Required(), mcp.Description("Step UUID")),
	), playStepHandler)

	s.AddTool(mcp.NewTool("replay_step",
		mcp.WithDescription("Fully reset a step and all its transitive descendants back to pending, clearing jobs/batches. Use to re-run a subtree."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
		mcp.WithString("step_id", mcp.Required(), mcp.Description("Step UUID")),
	), replayStepHandler)

	s.AddTool(mcp.NewTool("play_level",
		mcp.WithDescription("Unlock every step at a given DAG level in a run (manual dispatch mode)."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
		mcp.WithNumber("level", mcp.Required(), mcp.Description("DAG level (0-based)")),
	), playLevelHandler)

	s.AddTool(mcp.NewTool("replay_level",
		mcp.WithDescription("Fully reset every step at level >= the given level back to pending, clearing jobs/batches."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
		mcp.WithNumber("level", mcp.Required(), mcp.Description("DAG level (0-based)")),
	), replayLevelHandler)

	s.AddTool(mcp.NewTool("cancel_run",
		mcp.WithDescription("Cancel a run: mark it cancelled and kill all pending/running jobs."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
	), cancelRunHandler)

	s.AddTool(mcp.NewTool("retry_run",
		mcp.WithDescription("Retry a failed run: requeue dead/failed jobs and reset failing steps to pending."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run UUID")),
	), retryRunHandler)

	// ---- dev-loop helpers (Claude operator convenience) ----

	s.AddTool(mcp.NewTool("restart_api",
		mcp.WithDescription("Restart the squishy API container (`docker compose restart api`) so freshly-edited Go code is picked up. Returns the docker output and the API's recent logs so compile errors are visible. Use this after editing code under internal/ or cmd/squishy/."),
	), restartApiHandler)

	s.AddTool(mcp.NewTool("restart_mcp",
		mcp.WithDescription("Restart the squishy-mcp container itself. Use only when MCP-side handler/tool definitions changed; the call returns immediately, the connection drops, and the next tool invocation reconnects to the new instance."),
	), restartMcpHandler)

	s.AddTool(mcp.NewTool("run_unit_tests",
		mcp.WithDescription("Run the dockerized Go unit-test target (`docker compose --profile test run --rm unit-tests go test ...`). Defaults to internal/dialects/oracle and internal/translate. Use the `run` parameter to filter by test name pattern."),
		mcp.WithString("packages", mcp.Description("Space-separated Go package paths (default: './internal/dialects/oracle/... ./internal/translate/...').")),
		mcp.WithString("run", mcp.Description("Optional -run pattern to filter tests by name.")),
		mcp.WithNumber("timeout_seconds", mcp.Description("Test timeout in seconds (default 180, min 30, max 600).")),
		mcp.WithBoolean("verbose", mcp.Description("Pass -v to go test (default false).")),
	), runUnitTestsHandler)

	s.AddTool(mcp.NewTool("get_step_payload",
		mcp.WithDescription("Fetch a step's job payload from the app DB. Without `field`, returns a shape summary (per-key type and char length for string fields). With `field`, slices the named field via `offset` + `max_chars` (defaults to 4000-char windows). Use this to inspect e.g. the `script` field of a failed create_ddl step without dumping megabytes."),
		mcp.WithString("step_id", mcp.Required(), mcp.Description("Step UUID")),
		mcp.WithString("field", mcp.Description("Optional payload field to extract (e.g. 'script', 'table', 'schema').")),
		mcp.WithNumber("offset", mcp.Description("Char offset for the field slice (default 0).")),
		mcp.WithNumber("max_chars", mcp.Description("Max chars in the slice (default 4000, min 200).")),
	), getStepPayloadHandler)
}
