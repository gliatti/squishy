<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api, type GeneratedOverride, type MigrationStatus } from '../../api/client'

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

// ---- generated-column overrides (MySQL / MariaDB sources) ----
//
// Both maps come from typed migration data, never from explanation text:
// target_plan.tables[].copied_generated lists the generated columns squishy
// refused and creates as plain columns copied from the source, and
// generated_overrides holds the user's PostgreSQL generation expressions.
// Keys are '<table>.<column>', the shape of an explanation's object; a
// lookup falls back to a case-insensitive match (MySQL column names are
// case-insensitive, so the plan and an override may spell them apart).

type ColumnRef = { table: string; column: string }
type EditorHost = { target: ColumnRef; override?: GeneratedOverride; collapsed: boolean }

const copiedGenerated = ref(new Map<string, ColumnRef>())
const overrides = ref(new Map<string, GeneratedOverride>())
const overrideList = ref<GeneratedOverride[]>([])

function refKey(table: string, column: string): string {
  return `${table}.${column}`
}

function lookup<T>(m: Map<string, T>, object: string): T | undefined {
  const hit = m.get(object)
  if (hit !== undefined) return hit
  const folded = object.toLowerCase()
  for (const [k, v] of m) {
    if (k.toLowerCase() === folded) return v
  }
  return undefined
}

async function loadOverrideState() {
  const m = await api.getMigration(props.migrationId)
  const copied = new Map<string, ColumnRef>()
  const tables: any[] = Array.isArray(m.target_plan?.tables) ? m.target_plan.tables : []
  for (const t of tables) {
    for (const c of (t.copied_generated || []) as string[]) {
      copied.set(refKey(t.name, c), { table: t.name, column: c })
    }
  }
  const list = m.generated_overrides || []
  const ovs = new Map<string, GeneratedOverride>()
  for (const o of list) ovs.set(refKey(o.table, o.column), o)
  copiedGenerated.value = copied
  overrides.value = ovs
  overrideList.value = list
}

function hostKey(h: EditorHost): string {
  return refKey(h.target.table, h.target.column.toLowerCase())
}

// The explanation hosting the editor of each column: the first error of a
// refused (copied) column, else the first info of an overridden column,
// whose editor starts collapsed behind "Edit override".
const editorHosts = computed(() => {
  const hosts = new Map<number, EditorHost>()
  const seen = new Set<string>()
  explanations.value.forEach((e, i) => {
    const object: string = e.object || ''
    const copied = lookup(copiedGenerated.value, object)
    const ov = lookup(overrides.value, object)
    let host: EditorHost | undefined
    if (copied && e.level === 'error') {
      host = { target: copied, override: ov, collapsed: false }
    } else if (!copied && ov && e.level === 'info') {
      host = { target: { table: ov.table, column: ov.column }, override: ov, collapsed: true }
    }
    if (!host || seen.has(hostKey(host))) return
    seen.add(hostKey(host))
    hosts.set(i, host)
  })
  return hosts
})

const drafts = reactive<Record<string, string>>({})
const editorErrors = reactive<Record<string, string>>({})
const expanded = reactive<Record<string, boolean>>({})
const saving = ref(false)
const busy = computed(() => saving.value || loading.value)

function draftOf(h: EditorHost): string {
  return drafts[hostKey(h)] ?? h.override?.expression ?? ''
}
function isOpen(h: EditorHost): boolean {
  return !h.collapsed || !!expanded[hostKey(h)]
}
function closeEditor(h: EditorHost) {
  const k = hostKey(h)
  expanded[k] = false
  delete drafts[k]
  delete editorErrors[k]
}

function withoutTarget(target: ColumnRef): GeneratedOverride[] {
  const column = target.column.toLowerCase()
  return overrideList.value.filter(o => !(o.table === target.table && o.column.toLowerCase() === column))
}

// putAndReplan replaces the stored list, then re-plans so the DDL and the
// explanations reflect it. A refusal (the API's 400 carries PostgreSQL's
// error) stays under the editor.
async function putAndReplan(h: EditorHost, next: GeneratedOverride[]) {
  const k = hostKey(h)
  saving.value = true
  delete editorErrors[k]
  try {
    await api.setGeneratedOverrides(props.migrationId, next)
  } catch (e: any) {
    editorErrors[k] = e.message
    return
  } finally {
    saving.value = false
  }
  closeEditor(h)
  await plan()
}

async function saveOverride(h: EditorHost) {
  const next = withoutTarget(h.target)
  next.push({ table: h.target.table, column: h.target.column, expression: draftOf(h) })
  await putAndReplan(h, next)
}

async function removeOverride(h: EditorHost) {
  await putAndReplan(h, withoutTarget(h.target))
}

// ---- plan ----

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
      await loadOverrideState()
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
    // The plan response has no target_plan: read it and the stored
    // overrides back from the migration.
    await loadOverrideState()
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
      <button @click="plan" :disabled="busy">
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
        <li v-for="(e, i) in explanations" :key="i" :class="e.level === 'error' ? 'err' : (e.level === 'warn' ? 'warn' : '')">
          <strong v-if="e.level === 'error'">[error] </strong><strong>{{ e.object }}</strong>: <code>{{ e.source }}</code> → <code>{{ e.target }}</code> — {{ e.reason }}

          <template v-for="h in (editorHosts.has(i) ? [editorHosts.get(i)!] : [])" :key="hostKey(h)">
            <div v-if="!isOpen(h)" class="gen-override-toggle">
              <button class="secondary" :disabled="busy" @click="expanded[hostKey(h)] = true">Edit override</button>
            </div>
            <div v-else class="gen-override" :data-override-target="`${h.target.table}.${h.target.column}`">
              <label :for="`gen-ov-${i}`" class="gen-override-label">
                Generation override for <code>{{ h.target.table }}.{{ h.target.column }}</code>
              </label>
              <textarea
                :id="`gen-ov-${i}`"
                class="gen-override-input"
                rows="3"
                spellcheck="false"
                :disabled="busy"
                :value="draftOf(h)"
                @input="drafts[hostKey(h)] = ($event.target as HTMLTextAreaElement).value"
                placeholder="e.g. CASE WHEN &quot;paid&quot; THEN '1' ELSE '0' END"
              ></textarea>
              <div class="gen-override-hint">
                PostgreSQL expression, immutable, must print exactly what MySQL stores; validated on the target.
              </div>
              <div class="gen-override-actions">
                <button :disabled="busy || !draftOf(h).trim()" @click="saveOverride(h)">
                  {{ saving ? 'Saving…' : 'Save override' }}
                </button>
                <button v-if="h.override" class="secondary" :disabled="busy" @click="removeOverride(h)">Remove override</button>
                <button v-if="h.collapsed" class="secondary" :disabled="busy" @click="closeEditor(h)">Cancel</button>
              </div>
              <p v-if="editorErrors[hostKey(h)]" class="err gen-override-error">{{ editorErrors[hostKey(h)] }}</p>
            </div>
          </template>
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

<style scoped>
.gen-override-toggle {
  margin: 0.3rem 0 0.6rem;
}
.gen-override {
  margin: 0.4rem 0 0.8rem;
  padding: 0.6rem 0.75rem;
  background: #fff;
  border: 1px solid #eee;
  border-left: 3px solid currentColor;
  border-radius: 4px;
}
.gen-override-label {
  margin-top: 0;
  margin-bottom: 0.3rem;
}
.gen-override-input {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 0.85rem;
  color: #222;
  resize: vertical;
}
.gen-override-hint {
  font-size: 0.8rem;
  color: #666;
  margin: 0.3rem 0 0.5rem;
}
.gen-override-actions {
  display: flex;
  gap: 0.5rem;
  flex-wrap: wrap;
}
.gen-override-error {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  margin: 0.5rem 0 0;
}
</style>
