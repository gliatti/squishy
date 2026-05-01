<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, type Instance, type InstanceCreateReq, type SourceKind } from '../api/client'

const route = useRoute()
const router = useRouter()
const instanceID = route.params.iid as string

const loading = ref(true)
const submitting = ref(false)
const err = ref('')
const original = ref<Instance | null>(null)

// We reuse the create payload shape for symmetry. password / target_password
// are sent only when the user enters something (empty = keep existing).
const form = reactive<InstanceCreateReq>({
  name: '',
  kind: 'mysql' as SourceKind,
  host: '', port: 0, database: '', username: '', password: '', ssl_mode: 'disable',
  target_strategy: 'dedicated_db',
  target_host: '', target_port: 0, target_database: '', target_username: '',
  target_password: '', target_ssl_mode: 'disable',
  target_create_db: false,
})

async function load() {
  loading.value = true
  err.value = ''
  try {
    const res = await api.getInstance(instanceID)
    original.value = res.instance
    form.name = res.instance.name
    form.kind = res.instance.kind
    form.host = res.instance.host
    form.port = res.instance.port
    form.database = res.instance.database
    form.username = res.instance.username
    form.password = '' // never echoed back, leave empty unless user types
    form.ssl_mode = res.instance.ssl_mode
    form.target_strategy = res.instance.target_strategy
    form.target_host = res.instance.target_host
    form.target_port = res.instance.target_port
    form.target_database = res.instance.target_database
    form.target_username = res.instance.target_username
    form.target_password = '' // never echoed back
    form.target_ssl_mode = res.instance.target_ssl_mode
    form.target_create_db = res.instance.target_create_db
  } catch (e: any) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(load)

const targetDbHint = computed(() =>
  form.target_strategy === 'dedicated_db'
    ? 'admin DB used for CREATE DATABASE (typically "postgres")'
    : 'host DB for all migrated schemas')

async function submit() {
  err.value = ''
  if (!form.name.trim()) { err.value = 'name required'; return }
  if (!form.target_host.trim() || !form.target_database.trim() || !form.target_username.trim()) {
    err.value = 'target host / database / username required'
    return
  }
  submitting.value = true
  try {
    // Strip kind from the body (immutable server-side; sending it would be ignored anyway).
    const { kind: _kind, ...body } = form
    void _kind
    await api.updateInstance(instanceID, body)
    router.push(`/instances/${instanceID}`)
  } catch (e: any) {
    err.value = e.message
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section>
    <p><router-link :to="`/instances/${instanceID}`">← Back to instance</router-link></p>
    <h2>Edit instance</h2>

    <p v-if="loading" style="opacity:.7">Loading…</p>
    <p v-if="err" class="err">{{ err }}</p>

    <div v-if="!loading && original">
      <div class="card">
        <h3>1. Source endpoint</h3>
        <p style="opacity:.7">
          kind = <span class="pill">{{ original.kind }}</span> (immutable — changing the dialect would invalidate planned migrations)
        </p>
        <label>Display name</label>
        <input v-model="form.name" />

        <label>Host</label>     <input v-model="form.host" />
        <label>Port</label>     <input v-model.number="form.port" type="number" />
        <label>Database</label> <input v-model="form.database" />
        <label>Username</label> <input v-model="form.username" />
        <label>Password <small style="opacity:.7">(leave empty to keep existing)</small></label>
        <input v-model="form.password" type="password" />
        <label>SSL mode</label>
        <select v-model="form.ssl_mode">
          <option>disable</option><option>require</option><option>verify-full</option>
        </select>
      </div>

      <div class="card">
        <h3>2. Target mapping strategy</h3>
        <label style="display:flex; gap:.6rem; align-items:flex-start; cursor:pointer; margin-bottom:.4rem;">
          <input type="radio" value="dedicated_db" v-model="form.target_strategy" />
          <span><strong>One PG database per source schema</strong> — CREATE DATABASE on each migration. target_database below = admin DB.</span>
        </label>
        <label style="display:flex; gap:.6rem; align-items:flex-start; cursor:pointer;">
          <input type="radio" value="dedicated_schema" v-model="form.target_strategy" />
          <span><strong>One PG schema per source schema, all in one DB</strong> — schemas land in target_database below.</span>
        </label>
      </div>

      <div class="card">
        <h3>3. Target PostgreSQL endpoint</h3>
        <label>Host</label>     <input v-model="form.target_host" />
        <label>Port</label>     <input v-model.number="form.target_port" type="number" />
        <label>Database <small style="opacity:.7">({{ targetDbHint }})</small></label>
        <input v-model="form.target_database" />
        <label>Username</label> <input v-model="form.target_username" />
        <label>Password <small style="opacity:.7">(leave empty to keep existing)</small></label>
        <input v-model="form.target_password" type="password" />
        <label>SSL mode</label>
        <select v-model="form.target_ssl_mode">
          <option>disable</option><option>require</option><option>verify-full</option>
        </select>

        <label style="display:flex; gap:.6rem; align-items:flex-start; cursor:pointer; margin-top:.6rem;">
          <input type="checkbox" v-model="form.target_create_db" />
          <span>
            <strong>Create database if missing</strong> — squishy will issue <code>CREATE DATABASE</code> via the PG default DB <code>postgres</code> if <code>{{ form.target_database }}</code> doesn't exist.
          </span>
        </label>
      </div>

      <p>
        <button @click="submit" :disabled="submitting">{{ submitting ? 'Saving…' : 'Save' }}</button>
        &nbsp;<router-link :to="`/instances/${instanceID}`"><button class="secondary" :disabled="submitting">Cancel</button></router-link>
      </p>
    </div>
  </section>
</template>
