<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api, type DispatchMode, type MigrationRun, type MigrationStatus } from '../../api/client'

const props = defineProps<{
  migrationId: string
  status: MigrationStatus
}>()

const runs = ref<MigrationRun[]>([])
const err = ref('')
const launching = ref(false)
// `skipData=true` omits copy_table + create_index + create_fk so the run
// only re-runs routines / triggers / views against the schema and rows
// that a previous successful run already loaded. Lets the user iterate on
// routine-translation fixes in seconds instead of redoing the data copy.
const skipData = ref(false)

let iv: ReturnType<typeof setInterval> | null = null

async function refresh() {
  try {
    runs.value = (await api.listMigrationRuns(props.migrationId)).runs || []
  } catch (e: any) {
    err.value = e.message
  }
}

const hasActive = computed(() =>
  runs.value.some(r => r.status === 'pending' || r.status === 'running'))

async function launch(mode: DispatchMode) {
  launching.value = true
  err.value = ''
  try {
    await api.startRun(props.migrationId, mode, skipData.value)
    await refresh()
  } catch (e: any) {
    err.value = e.message
  } finally {
    launching.value = false
  }
}

function statusPill(s: string): string {
  if (s === 'succeeded') return 'pill ok'
  if (s === 'running' || s === 'pending') return 'pill run'
  if (s === 'failed' || s === 'cancelled') return 'pill fail'
  return 'pill'
}

function pct(done: number, total: number): string {
  if (!total) return '0 %'
  return `${Math.floor((done / total) * 100)} %`
}

function fmtDate(s: string): string {
  if (!s || s.startsWith('1970-')) return '—'
  const d = new Date(s)
  return d.toLocaleString()
}

function duration(start: string, end: string): string {
  if (!start || start.startsWith('1970-')) return '—'
  const s = new Date(start).getTime()
  const e = (!end || end.startsWith('1970-')) ? Date.now() : new Date(end).getTime()
  const ms = e - s
  if (ms < 1000) return `${ms} ms`
  const sec = Math.floor(ms / 1000)
  if (sec < 60) return `${sec} s`
  const min = Math.floor(sec / 60)
  const remSec = sec % 60
  if (min < 60) return `${min}m ${remSec}s`
  const h = Math.floor(min / 60)
  return `${h}h ${min % 60}m`
}

onMounted(() => {
  refresh()
  // Refresh every 5s while there's an active run.
  iv = setInterval(() => {
    if (hasActive.value) refresh()
  }, 5000)
})
onUnmounted(() => {
  if (iv) clearInterval(iv)
})
</script>

<template>
  <div>
    <p v-if="status === 'draft'" style="opacity:.7">
      No plan yet — open the <strong>DDL</strong> tab and click "Plan migration" first.
    </p>

    <div v-else>
      <p>Launch a new run for this migration. <strong>auto</strong> dispatches the whole DAG ; <strong>manual</strong> requires unlocking each level by hand from the run page.</p>

      <p v-if="err" class="err">{{ err }}</p>

      <p style="margin: .25rem 0 .5rem 0;">
        <label style="user-select:none; cursor:pointer;">
          <input type="checkbox" v-model="skipData" :disabled="launching || hasActive" />
          &nbsp;<strong>skip data</strong>
          <span style="opacity:.7; margin-left:.4rem;">
            (omit copy_table / create_index / create_fk — re-run only routines, triggers, views.
             Use after a successful first run to iterate on body translation in seconds.)
          </span>
        </label>
      </p>
      <p>
        <button @click="launch('auto')" :disabled="launching || hasActive">
          {{ launching ? 'Launching…' : '▶ Launch (auto)' }}
        </button>
        &nbsp;
        <button class="secondary" @click="launch('manual')" :disabled="launching || hasActive">
          ▶ Launch (manual)
        </button>
        <span v-if="hasActive" style="margin-left:.6rem; opacity:.7">
          a run is already active — wait for it to finish or cancel it from the run page.
        </span>
      </p>

      <h3 style="margin-top:1.5rem">Runs ({{ runs.length }})</h3>
      <p v-if="!runs.length" style="opacity:.7">No runs yet for this migration.</p>
      <table v-else style="width:100%; border-collapse:collapse;">
        <thead>
          <tr style="text-align:left; border-bottom:1px solid #ddd;">
            <th style="padding:.4rem;">Status</th>
            <th style="padding:.4rem;">Mode</th>
            <th style="padding:.4rem;">Started</th>
            <th style="padding:.4rem;">Duration</th>
            <th style="padding:.4rem;">Steps</th>
            <th style="padding:.4rem;">Rows</th>
            <th style="padding:.4rem;"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in runs" :key="r.id" style="border-bottom:1px solid #eee;">
            <td style="padding:.4rem;"><span :class="statusPill(r.status)">{{ r.status }}</span></td>
            <td style="padding:.4rem;"><span class="pill">{{ r.dispatch_mode }}</span></td>
            <td style="padding:.4rem;">{{ fmtDate(r.started_at) }}</td>
            <td style="padding:.4rem;">{{ duration(r.started_at, r.finished_at) }}</td>
            <td style="padding:.4rem;">
              {{ r.steps_done }}/{{ r.steps_total }}
              <span v-if="r.steps_failed > 0" class="err">&nbsp;({{ r.steps_failed }} fail)</span>
            </td>
            <td style="padding:.4rem;">
              {{ r.rows_done }} / {{ r.rows_total }}
              <span style="opacity:.7">({{ pct(r.rows_done, r.rows_total) }})</span>
            </td>
            <td style="padding:.4rem;">
              <router-link :to="`/runs/${r.id}`">Open →</router-link>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
