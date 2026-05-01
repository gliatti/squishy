<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, type InstanceCreateReq, type SourceKind } from '../api/client'

const route = useRoute()
const router = useRouter()
const projectID = route.params.id as string

type DialectMeta = {
  value: SourceKind
  label: string
  defaults: { host: string; port: number; database: string; username: string; password: string }
  dbHint: string
}

const dialects: DialectMeta[] = [
  {
    value: 'mysql',  label: 'MySQL 8',
    defaults: { host: 'mysql-sample', port: 3306, database: 'sakila', username: 'root', password: 'root' },
    dbHint: 'default DB for the connection (any non-system one works — schemas are discovered)',
  },
  {
    value: 'mariadb', label: 'MariaDB 10/11',
    defaults: { host: 'mysql-sample', port: 3306, database: 'sakila', username: 'root', password: 'root' },
    dbHint: 'default DB for the connection',
  },
  {
    value: 'oracle',  label: 'Oracle Database 23ai',
    defaults: { host: 'oracle-sample', port: 1521, database: 'FREEPDB1', username: 'system', password: 'oracle' },
    dbHint: 'service name (PDB), e.g. FREEPDB1',
  },
  {
    value: 'oracle19', label: 'Oracle Database 19c',
    defaults: { host: 'oracle19-sample', port: 1521, database: 'ORCLPDB1', username: 'system', password: 'oracle' },
    dbHint: 'service name (PDB), e.g. ORCLPDB1',
  },
  {
    value: 'db2', label: 'IBM DB2 11.5 (LUW)',
    defaults: { host: 'db2-sample', port: 50000, database: 'SAMPLE', username: 'db2inst1', password: 'password' },
    dbHint: 'database name (max 8 chars), e.g. SAMPLE',
  },
  {
    value: 'db2zos', label: 'IBM DB2 for z/OS',
    defaults: { host: '', port: 446, database: '', username: '', password: '' },
    dbHint: 'subsystem location name (DSN, DB2A, …)',
  },
]

const form = reactive<InstanceCreateReq>({
  name: '',
  kind: 'mysql',
  host: dialects[0].defaults.host,
  port: dialects[0].defaults.port,
  database: dialects[0].defaults.database,
  username: dialects[0].defaults.username,
  password: dialects[0].defaults.password,
  ssl_mode: 'disable',
  target_strategy: 'dedicated_db',
  target_host: 'postgres',
  target_port: 5432,
  target_database: 'postgres',
  target_username: 'squishy',
  target_password: 'squishy',
  target_ssl_mode: 'disable',
  target_create_db: false,
})

watch(() => form.kind, (newKind, oldKind) => {
  if (!oldKind || newKind === oldKind) return
  const d = dialects.find(d => d.value === newKind)
  if (!d) return
  form.host = d.defaults.host; form.port = d.defaults.port; form.database = d.defaults.database
  form.username = d.defaults.username; form.password = d.defaults.password
  if (!form.name) form.name = `${newKind}-${d.defaults.host}`
})

// When the strategy switches, suggest a sensible default for target_database:
//   dedicated_db     → "postgres" (admin DB)
//   dedicated_schema → "migrated"  (host DB for all schemas)
watch(() => form.target_strategy, (s) => {
  if (s === 'dedicated_db' && (form.target_database === 'migrated' || !form.target_database)) {
    form.target_database = 'postgres'
  } else if (s === 'dedicated_schema' && (form.target_database === 'postgres' || !form.target_database)) {
    form.target_database = 'migrated'
  }
})

const dbHint = computed(() => dialects.find(d => d.value === form.kind)?.dbHint ?? '')

const targetDbHint = computed(() =>
  form.target_strategy === 'dedicated_db'
    ? 'admin DB (squishy will issue CREATE DATABASE on it — typically "postgres")'
    : 'host DB for all migrated schemas (e.g. "migrated")')

const err = ref('')
const submitting = ref(false)

async function submit() {
  err.value = ''
  if (!form.name.trim()) { err.value = 'name required'; return }
  if (!form.target_host.trim() || !form.target_database.trim() || !form.target_username.trim()) {
    err.value = 'target host / database / username required'
    return
  }
  submitting.value = true
  try {
    const res = await api.createInstance(projectID, { ...form })
    if (res.discover_error) {
      alert(`Instance created, but discovery failed:\n${res.discover_error}\nYou can re-try from the instance page.`)
    }
    router.push(`/instances/${res.instance.id}`)
  } catch (e: any) {
    err.value = e.message
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section>
    <p><router-link :to="`/projects/${projectID}`">← Back to project</router-link></p>
    <h2>Add instance</h2>

    <div class="card">
      <h3>1. Source endpoint</h3>
      <label>Display name</label>
      <input v-model="form.name" placeholder="oracle-prod" />

      <label>Kind</label>
      <select v-model="form.kind">
        <option v-for="d in dialects" :key="d.value" :value="d.value">{{ d.label }}</option>
      </select>

      <label>Host</label>     <input v-model="form.host" />
      <label>Port</label>     <input v-model.number="form.port" type="number" />
      <label>Database <small style="opacity:.7">({{ dbHint }})</small></label>
      <input v-model="form.database" />
      <label>Username (super-user)</label> <input v-model="form.username" />
      <label>Password</label> <input v-model="form.password" type="password" />
    </div>

    <div class="card">
      <h3>2. Target mapping strategy</h3>
      <p style="opacity:.7">Each non-system schema discovered on the source becomes one migration. Choose how those migrations land in PostgreSQL:</p>
      <label style="display:flex; gap:.6rem; align-items:flex-start; cursor:pointer; margin-bottom:.4rem;">
        <input type="radio" value="dedicated_db" v-model="form.target_strategy" />
        <span>
          <strong>One PG database per source schema</strong> — squishy issues <code>CREATE DATABASE &lt;source_schema&gt;</code> via the target endpoint below. The target user must hold <code>CREATEDB</code>. The target_database below is the admin DB (typically <code>postgres</code>).
        </span>
      </label>
      <label style="display:flex; gap:.6rem; align-items:flex-start; cursor:pointer;">
        <input type="radio" value="dedicated_schema" v-model="form.target_strategy" />
        <span>
          <strong>One PG schema per source schema, all in a single PG database</strong> — squishy creates the schemas inside the target_database below.
        </span>
      </label>
    </div>

    <div class="card">
      <h3>3. Target PostgreSQL endpoint</h3>
      <p style="opacity:.7">PG super-user used by squishy for this instance.</p>
      <label>Host</label>     <input v-model="form.target_host" />
      <label>Port</label>     <input v-model.number="form.target_port" type="number" />
      <label>Database <small style="opacity:.7">({{ targetDbHint }})</small></label>
      <input v-model="form.target_database" />
      <label>Username</label> <input v-model="form.target_username" />
      <label>Password</label> <input v-model="form.target_password" type="password" />
      <label>SSL mode</label>
      <select v-model="form.target_ssl_mode">
        <option>disable</option><option>require</option><option>verify-full</option>
      </select>

      <label style="display:flex; gap:.6rem; align-items:flex-start; cursor:pointer; margin-top:.6rem;">
        <input type="checkbox" v-model="form.target_create_db" />
        <span>
          <strong>Create database if missing</strong> —
          if this is checked and <code>{{ form.target_database }}</code> doesn't exist, squishy connects on the PG default DB <code>postgres</code> and runs <code>CREATE DATABASE</code>. Requires the user to hold <code>CREATEDB</code>.
        </span>
      </label>
    </div>

    <p v-if="err" class="err">{{ err }}</p>
    <p>
      <button @click="submit" :disabled="submitting">
        {{ submitting ? 'Creating + discovering…' : 'Create instance' }}
      </button>
    </p>
  </section>
</template>
