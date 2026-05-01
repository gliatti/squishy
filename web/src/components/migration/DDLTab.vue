<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, type MigrationStatus } from '../../api/client'

const props = defineProps<{
  migrationId: string
  status: MigrationStatus
}>()
const emit = defineEmits<{ (e: 'planned'): void }>()

const loading = ref(false)
const err = ref('')
const ddl = ref('')
const explanations = ref<any[]>([])
const warnings = ref<any[]>([])
const planned = ref(false)

async function loadFromMigration() {
  loading.value = true
  err.value = ''
  try {
    const m = await api.getMigration(props.migrationId)
    if (m.status === 'planned') {
      ddl.value = (m.ddl_script || '') + '\n\n-- ------ post-copy -------\n\n' + (m.ddl_post_script || '')
      explanations.value = (m.explanations as any[]) || []
      warnings.value = (m.warnings as any[]) || []
      planned.value = true
    } else {
      planned.value = false
    }
  } catch (e: any) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}

async function plan() {
  loading.value = true
  err.value = ''
  try {
    const res = await api.planMigration(props.migrationId)
    ddl.value = (res.ddl_script || '') + '\n\n-- ------ post-copy -------\n\n' + (res.ddl_post_script || '')
    explanations.value = res.explanations || []
    warnings.value = res.warnings || []
    planned.value = true
    emit('planned')
  } catch (e: any) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}

onMounted(loadFromMigration)
</script>

<template>
  <div>
    <p style="margin:0.5rem 0;">
      <button @click="plan" :disabled="loading">
        {{ loading ? 'Planning…' : (planned ? 'Re-plan' : 'Plan migration') }}
      </button>
      <span v-if="!planned && !loading" style="margin-left:.5rem; opacity:.7">
        No plan yet — click "Plan migration" to inspect the source and generate the DDL.
      </span>
    </p>
    <p v-if="err" class="err">{{ err }}</p>

    <div v-if="planned">
      <h3>Generated PostgreSQL DDL</h3>
      <pre>{{ ddl }}</pre>

      <h3>Explanations ({{ explanations.length }})</h3>
      <ul v-if="explanations.length">
        <li v-for="(e, i) in explanations" :key="i" :class="e.level === 'warn' ? 'warn' : ''">
          <strong>{{ e.object }}</strong>: <code>{{ e.source }}</code> → <code>{{ e.target }}</code> — {{ e.reason }}
        </li>
      </ul>
      <p v-else style="opacity:.7">None.</p>

      <h3 v-if="warnings.length" class="warn">Warnings ({{ warnings.length }})</h3>
      <ul v-if="warnings.length">
        <li v-for="(w, i) in warnings" :key="i" class="warn">
          <strong>{{ w.object }}</strong> [{{ w.kind }}] — {{ w.message }}
        </li>
      </ul>
    </div>
  </div>
</template>
