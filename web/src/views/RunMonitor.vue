<script setup lang="ts">
import { onMounted, onUnmounted, ref, shallowRef, triggerRef } from 'vue'
import { useRoute } from 'vue-router'
import { api, type Step, type Batch } from '../api/client'
import { subscribe, type StreamEvent } from '../api/stream'
import LevelsView from '../components/run/LevelsView.vue'
import TreeView from '../components/run/TreeView.vue'

const route = useRoute()
const runID = route.params.id as string

type Tab = 'levels' | 'tree'
const TAB_KEY = 'squishy.runMonitor.tab'
const tab = ref<Tab>(((): Tab => {
  const v = localStorage.getItem(TAB_KEY)
  return v === 'tree' ? 'tree' : 'levels'
})())
function setTab(t: Tab) {
  tab.value = t
  localStorage.setItem(TAB_KEY, t)
}

const run = ref<any>(null)
const steps = ref<Step[]>([])
const logs = ref<StreamEvent[]>([])

// batchesByStep is shallowRef of a Map — we triggerRef after mutating.
const batchesByStep = shallowRef<Map<string, Batch[]>>(new Map())
const expandedSteps = ref<Set<string>>(new Set())
const expandedNodes = ref<Set<string>>(new Set())

let unsub: () => void = () => {}
let iv: ReturnType<typeof setInterval> | null = null

async function refresh() {
  try {
    run.value = await api.getRun(runID)
    steps.value = (await api.listSteps(runID)).steps || []
    // refresh any batches the user had expanded
    for (const stepId of expandedSteps.value) {
      await loadBatches(stepId)
    }
  } catch { /* server might still be flipping run status */ }
}

async function loadBatches(stepId: string) {
  try {
    const res = await api.listBatches(runID, stepId)
    batchesByStep.value.set(stepId, res.batches || [])
    triggerRef(batchesByStep)
  } catch { /* ignore */ }
}

async function onToggleBatches(stepId: string) {
  if (expandedSteps.value.has(stepId)) {
    expandedSteps.value.delete(stepId)
  } else {
    expandedSteps.value.add(stepId)
    if (!batchesByStep.value.has(stepId)) {
      await loadBatches(stepId)
    }
  }
}

function onToggleNode(stepId: string) {
  if (expandedNodes.value.has(stepId)) expandedNodes.value.delete(stepId)
  else expandedNodes.value.add(stepId)
}

function onExpandAll() {
  expandedNodes.value = new Set(steps.value.map(s => s.id))
}
function onCollapseAll() {
  expandedNodes.value = new Set()
}

onMounted(() => {
  refresh()
  iv = setInterval(refresh, 5000)
  unsub = subscribe(runID, (e) => {
    logs.value.unshift(e)
    if (logs.value.length > 200) logs.value.length = 200
    if (e.kind === 'run.status' || e.kind === 'step.status') refresh()
    if (e.kind === 'batch.progress') {
      // refresh batches for any expanded step (cheap enough vs. parsing msg)
      for (const stepId of expandedSteps.value) loadBatches(stepId)
    }
  })
})
onUnmounted(() => {
  if (iv) clearInterval(iv)
  unsub()
})

function pillClass(status: string): string {
  if (status === 'succeeded') return 'pill ok'
  if (status === 'running') return 'pill run'
  if (status === 'failed' || status === 'cancelled' || status === 'dead') return 'pill fail'
  return 'pill'
}

async function retry() { await api.retryRun(runID); refresh() }
async function cancel() { await api.cancelRun(runID); refresh() }
async function onPlayLevel(level: number) {
  try { await api.playLevel(runID, level) } finally { refresh() }
}
async function onReplayLevel(level: number) {
  if (!confirm(`Réinitialiser et relancer le niveau ${level} ?`)) return
  try { await api.replayLevel(runID, level) } finally { refresh() }
}
async function onReplayStep(stepId: string) {
  try { await api.replayStep(runID, stepId) } finally { refresh() }
}
async function onPlayStep(stepId: string) {
  try { await api.playStep(runID, stepId) } finally { refresh() }
}
</script>

<template>
  <section>
    <h2>Run <code>{{ runID }}</code></h2>
    <div class="card">
      <p>Status: <span :class="pillClass(run?.status || 'pending')">{{ run?.status || '…' }}</span>
         &nbsp;· mode: <span class="pill">{{ run?.dispatch_mode || 'auto' }}</span>
         &nbsp;· steps: {{ run?.steps_done || 0 }}/{{ run?.steps_total || 0 }}
         &nbsp;· rows: {{ run?.rows_done || 0 }}/{{ run?.rows_total || 0 }}
      </p>
      <p>
        <button class="secondary" @click="retry">Retry failed</button>
        &nbsp;<button class="secondary" @click="cancel">Cancel</button>
      </p>
    </div>

    <div class="card">
      <nav class="tabs" role="tablist">
        <button role="tab" :class="{ active: tab === 'levels' }" :aria-selected="tab === 'levels'" @click="setTab('levels')">Niveaux</button>
        <button role="tab" :class="{ active: tab === 'tree' }" :aria-selected="tab === 'tree'" @click="setTab('tree')">Arborescence</button>
      </nav>

      <LevelsView
        v-if="tab === 'levels'"
        :steps="steps"
        :batches-by-step="batchesByStep"
        :expanded-steps="expandedSteps"
        @toggle-batches="onToggleBatches"
        @play="onPlayLevel"
        @replay="onReplayLevel"
        @replay-step="onReplayStep"
        @play-step="onPlayStep"
      />

      <TreeView
        v-else
        :steps="steps"
        :batches-by-step="batchesByStep"
        :expanded-steps="expandedSteps"
        :expanded-nodes="expandedNodes"
        @toggle-batches="onToggleBatches"
        @toggle-node="onToggleNode"
        @replay-step="onReplayStep"
        @play-step="onPlayStep"
        @expand-all="onExpandAll"
        @collapse-all="onCollapseAll"
      />
    </div>

    <div class="card">
      <h3>Live events</h3>
      <div class="log">
        <div v-for="(e, i) in logs" :key="i">
          <span :class="e.level === 'error' ? 'err' : (e.level === 'warn' ? 'warn' : '')">
            [{{ new Date(e.ts).toLocaleTimeString() }}] {{ e.kind }} — {{ e.message }}
          </span>
        </div>
      </div>
    </div>
  </section>
</template>
