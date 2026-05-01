const BASE = '/api/v1'

export type StepStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'skipped' | 'cancelled'
export type DispatchMode = 'auto' | 'manual'

export interface Step {
  id: string
  seq: number
  kind: string
  target: string | null
  status: StepStatus
  attempts: number
  rows_total: number
  rows_done: number
  started_at: string
  finished_at: string
  error: string
  last_error: string
  depends_on: string[]
  level: number
  priority: number
  unlocked: boolean
  job_attempts: number
  job_max_attempts: number
}

export interface Batch {
  id: string
  seq: number
  range_kind: string
  range_low: unknown
  range_high: unknown
  row_count_est: number
  row_count: number
  status: StepStatus
  attempts: number
  started_at: string
  finished_at: string
  error: string
}

export interface Project {
  id: string
  name: string
  slug: string
  description: string
  created_at: string
  updated_at: string
}

export type SourceKind = 'mysql' | 'mariadb' | 'oracle' | 'oracle19' | 'db2' | 'db2zos'
export type TargetStrategy = 'dedicated_db' | 'dedicated_schema'

export interface Instance {
  id: string
  project_id: string
  name: string
  // Source DSN.
  kind: SourceKind
  host: string
  port: number
  database: string
  username: string
  ssl_mode: string
  params?: Record<string, unknown>
  // Mapping strategy + target PG DSN.
  target_strategy: TargetStrategy
  target_host: string
  target_port: number
  target_database: string
  target_username: string
  target_ssl_mode: string
  target_params?: Record<string, unknown>
  target_create_db: boolean
  created_at: string
  updated_at: string
}

export interface InstanceCreateReq {
  name: string
  // Source DSN.
  kind: SourceKind
  host: string
  port: number
  database: string
  username: string
  password: string
  ssl_mode: string
  // Mapping strategy + target PG DSN.
  target_strategy: TargetStrategy
  target_host: string
  target_port: number
  target_database: string
  target_username: string
  target_password: string
  target_ssl_mode: string
  target_create_db: boolean
}

export type MigrationStatus = 'draft' | 'planned'

export type RunStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'

export interface MigrationRun {
  id: string
  status: RunStatus
  dispatch_mode: DispatchMode
  triggered_by: string
  started_at: string
  finished_at: string
  error: string
  created_at: string
  steps_total: number
  steps_done: number
  steps_failed: number
  steps_running: number
  rows_total: number
  rows_done: number
}

export interface Migration {
  id: string
  instance_id: string
  source_schema_name: string
  target_db_name: string
  target_schema_name: string
  status: MigrationStatus
  ddl_script?: string
  ddl_post_script?: string
  source_schema?: any
  target_plan?: any
  data_plan?: any
  type_mappings?: any[]
  explanations?: any[]
  warnings?: any[]
  prerequisites?: any[]
  acked_prereqs?: string[]
  options?: any
  created_at: string
  updated_at: string
  // Set by ListMigrationsByInstance — pointer to the most recent run.
  latest_run_id?: string
  latest_run_status?: '' | RunStatus
}

async function req<T>(method: string, path: string, body?: any): Promise<T> {
  const res = await fetch(BASE + path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    let msg = res.statusText
    try { msg = (await res.json()).error ?? msg } catch { /* noop */ }
    throw new Error(msg)
  }
  if (res.status === 204) return undefined as unknown as T
  return res.json() as Promise<T>
}

export const api = {
  // ---- projects ----
  listProjects: () => req<{ projects: Project[] }>('GET', '/projects'),
  createProject: (body: { name: string; description?: string }) =>
    req<Project>('POST', '/projects', body),
  getProject: (id: string) => req<Project>('GET', `/projects/${id}`),
  updateProject: (id: string, body: { name: string; description?: string }) =>
    req<Project>('PUT', `/projects/${id}`, body),
  deleteProject: (id: string) => req<void>('DELETE', `/projects/${id}`),

  // ---- instances ----
  listInstances: (projectID: string) =>
    req<{ instances: Instance[] }>('GET', `/projects/${projectID}/instances`),
  createInstance: (projectID: string, body: InstanceCreateReq) =>
    req<{ instance: Instance; migrations: Migration[]; discover_error?: string }>(
      'POST', `/projects/${projectID}/instances`, body),
  getInstance: (instanceID: string) =>
    req<{ instance: Instance; migrations: Migration[] }>('GET', `/instances/${instanceID}`),
  deleteInstance: (instanceID: string) =>
    req<void>('DELETE', `/instances/${instanceID}`),
  updateInstance: (instanceID: string, body: Partial<InstanceCreateReq>) =>
    req<Instance>('PUT', `/instances/${instanceID}`, body),
  testInstanceConnection: (instanceID: string) =>
    req<{ ok: boolean; version?: string; message: string }>(
      'POST', `/instances/${instanceID}/test-connection`),
  testInstanceTargetConnection: (instanceID: string) =>
    req<{ ok: boolean; version?: string; message: string }>(
      'POST', `/instances/${instanceID}/test-target-connection`),
  rediscoverInstance: (instanceID: string) =>
    req<{ migrations: Migration[]; discover_error?: string }>(
      'POST', `/instances/${instanceID}/rediscover`),
  listInstanceMigrations: (instanceID: string) =>
    req<{ migrations: Migration[] }>('GET', `/instances/${instanceID}/migrations`),

  // ---- migrations ----
  getMigration: (id: string) => req<Migration>('GET', `/migrations/${id}`),
  inspectMigration: (id: string) => req<any>('POST', `/migrations/${id}/inspect`),
  planMigration: (id: string, opts: any = {}) =>
    req<any>('POST', `/migrations/${id}/plan`, { options: opts }),
  getPrerequisites: (id: string) => req<any>('GET', `/migrations/${id}/prerequisites`),
  ackPrerequisites: (id: string, acked: string[]) =>
    req<any>('POST', `/migrations/${id}/prerequisites/ack`, { acked }),
  startRun: (migrationID: string, mode: DispatchMode = 'auto', skipData = false) =>
    req<{ run_id: string }>('POST', `/migrations/${migrationID}/runs`, { mode, skip_data: skipData }),
  listMigrationRuns: (migrationID: string) =>
    req<{ runs: MigrationRun[] }>('GET', `/migrations/${migrationID}/runs`),

  // ---- runs ----
  getRun:      (id: string) => req<any>('GET', `/runs/${id}`),
  listSteps:   (id: string) => req<{ steps: Step[] }>('GET', `/runs/${id}/steps`),
  listBatches: (runID: string, stepID: string) =>
    req<{ batches: Batch[] }>('GET', `/runs/${runID}/steps/${stepID}/batches`),
  retryRun:    (id: string) => req<any>('POST', `/runs/${id}/retry`),
  cancelRun:   (id: string) => req<any>('POST', `/runs/${id}/cancel`),
  playLevel:   (runID: string, level: number) =>
    req<{ ok: boolean; level: number }>('POST', `/runs/${runID}/levels/${level}/play`),
  replayLevel: (runID: string, level: number) =>
    req<{ ok: boolean; level: number }>('POST', `/runs/${runID}/levels/${level}/replay`),
  playStep:    (runID: string, stepID: string) =>
    req<{ ok: boolean; step_id: string }>('POST', `/runs/${runID}/steps/${stepID}/play`),
  replayStep:  (runID: string, stepID: string) =>
    req<{ ok: boolean; step_id: string }>('POST', `/runs/${runID}/steps/${stepID}/replay`),
}
