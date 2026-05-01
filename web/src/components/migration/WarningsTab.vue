<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, type MigrationStatus } from '../../api/client'

type Prereq = {
  id: string
  severity: 'blocking' | 'info'
  category: 'install_extension' | 'manual_sql' | 'manual_review' | 'fix_source'
  object?: string
  title: string
  description: string
  remediation: string
}

const props = defineProps<{
  migrationId: string
  status: MigrationStatus
}>()

const loading = ref(true)
const err = ref('')
const prereqs = ref<Prereq[]>([])
const acked = ref<Set<string>>(new Set())
const saving = ref(false)

async function load() {
  loading.value = true
  err.value = ''
  try {
    const data = await api.getPrerequisites(props.migrationId)
    prereqs.value = data.prerequisites || []
    acked.value = new Set<string>(data.acked || [])
  } catch (e: any) {
    err.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(load)

const blocking = computed(() => prereqs.value.filter(p => p.severity === 'blocking'))
const info     = computed(() => prereqs.value.filter(p => p.severity === 'info'))
const unresolved = computed(() =>
  blocking.value.filter(p => !acked.value.has(p.id)).length)

async function toggle(p: Prereq) {
  if (acked.value.has(p.id)) acked.value.delete(p.id)
  else acked.value.add(p.id)
  await persist()
}

async function persist() {
  saving.value = true
  try {
    await api.ackPrerequisites(props.migrationId, [...acked.value])
  } catch (e: any) { err.value = e.message }
  finally { saving.value = false }
}

function categoryLabel(c: string): string {
  switch (c) {
    case 'install_extension': return 'Install extension'
    case 'manual_sql':        return 'Manual SQL'
    case 'manual_review':     return 'Manual review'
    case 'fix_source':        return 'Fix source'
  }
  return c
}
</script>

<template>
  <div>
    <p v-if="status === 'draft'" style="opacity:.7">
      No plan yet — open the <strong>DDL</strong> tab and click "Plan migration" first.
    </p>

    <div v-else>
      <p>Address each <strong>blocking</strong> item below (install extensions, translate routine bodies, fix source errors). Check each one to acknowledge once done. All blocking items must be acknowledged before launching the migration.</p>

      <div v-if="loading">Loading prerequisites…</div>
      <p v-if="err" class="err">{{ err }}</p>

      <section v-if="!loading">
        <h3>
          <span :class="unresolved ? 'pill fail' : 'pill ok'">
            {{ blocking.length - unresolved }}/{{ blocking.length }} blocking
          </span>
          &nbsp;<span class="pill">{{ info.length }} info</span>
          <span v-if="saving" style="margin-left:.5rem; opacity:.7;">saving…</span>
        </h3>

        <div v-if="!prereqs.length" class="card" style="margin-top:.6rem">
          <p>No prerequisites detected — you can proceed straight to the Execution tab.</p>
        </div>

        <div v-for="p in blocking" :key="p.id" class="card" style="margin-top:.6rem">
          <label style="display:flex; align-items:flex-start; gap:0.6rem; cursor:pointer;">
            <input type="checkbox" :checked="acked.has(p.id)" @change="toggle(p)" />
            <div style="flex:1">
              <strong class="err">⚠ {{ p.title }}</strong>
              &nbsp;<span class="pill">{{ categoryLabel(p.category) }}</span>
              <div v-if="p.object" style="font-size:0.8rem; color:#666;">{{ p.object }}</div>
              <p style="margin-top:0.4rem">{{ p.description }}</p>
              <pre v-if="p.remediation && p.remediation.trim()" style="white-space:pre-wrap; word-break:break-word; overflow-wrap:anywhere; max-width:100%;">{{ p.remediation }}</pre>
            </div>
          </label>
        </div>

        <div v-for="p in info" :key="p.id" class="card" style="margin-top:.6rem">
          <label style="display:flex; align-items:flex-start; gap:0.6rem; cursor:pointer;">
            <input type="checkbox" :checked="acked.has(p.id)" @change="toggle(p)" />
            <div style="flex:1">
              <strong>ℹ {{ p.title }}</strong>
              &nbsp;<span class="pill">{{ categoryLabel(p.category) }}</span>
              <div v-if="p.object" style="font-size:0.8rem; color:#666;">{{ p.object }}</div>
              <p style="margin-top:0.4rem">{{ p.description }}</p>
              <pre v-if="p.remediation && p.remediation.trim()" style="white-space:pre-wrap; word-break:break-word; overflow-wrap:anywhere; max-width:100%;">{{ p.remediation }}</pre>
            </div>
          </label>
        </div>
      </section>
    </div>
  </div>
</template>
