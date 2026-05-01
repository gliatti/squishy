<script setup lang="ts">
import { computed } from 'vue'
import type { Step, Batch } from '../../api/client'
import TreeNode from './TreeNode.vue'

const props = defineProps<{
  steps: Step[]
  batchesByStep: Map<string, Batch[]>
  expandedSteps: Set<string>
  expandedNodes: Set<string>
}>()
const emit = defineEmits<{
  (e: 'toggle-batches', stepId: string): void
  (e: 'toggle-node', stepId: string): void
  (e: 'replay-step', stepId: string): void
  (e: 'play-step', stepId: string): void
  (e: 'expand-all'): void
  (e: 'collapse-all'): void
}>()

// childrenMap: parent step id → list of direct child steps in the spanning-tree
// representation. A step with multiple parents is attached under the parent
// with the highest level (deepest in the DAG) so each step appears once.
const childrenMap = computed(() => {
  const byID = new Map<string, Step>()
  for (const s of props.steps) byID.set(s.id, s)

  const chosenParent = new Map<string, string | null>()
  for (const s of props.steps) {
    if (!s.depends_on || s.depends_on.length === 0) {
      chosenParent.set(s.id, null)
      continue
    }
    let best: Step | null = null
    for (const pid of s.depends_on) {
      const p = byID.get(pid)
      if (!p) continue
      if (!best || (p.level ?? 0) > (best.level ?? 0)) best = p
    }
    chosenParent.set(s.id, best ? best.id : null)
  }
  const m = new Map<string, Step[]>()
  for (const s of props.steps) {
    const pid = chosenParent.get(s.id)
    if (!pid) continue
    if (!m.has(pid)) m.set(pid, [])
    m.get(pid)!.push(s)
  }
  for (const arr of m.values()) {
    arr.sort((a, b) => (a.level - b.level) || (a.priority - b.priority) || (a.seq - b.seq))
  }
  return m
})

const roots = computed(() => {
  const rs = props.steps.filter(s => !s.depends_on || s.depends_on.length === 0)
  rs.sort((a, b) => (a.priority - b.priority) || (a.seq - b.seq))
  return rs
})
</script>

<template>
  <div>
    <div class="tree-toolbar">
      <button class="secondary" @click="emit('expand-all')">Tout déplier</button>
      <button class="secondary" @click="emit('collapse-all')">Tout replier</button>
    </div>
    <div class="tree-root">
      <TreeNode
        v-for="r in roots"
        :key="r.id"
        :step="r"
        :children-map="childrenMap"
        :batches-by-step="props.batchesByStep"
        :expanded-steps="props.expandedSteps"
        :expanded-nodes="props.expandedNodes"
        @toggle-batches="(id) => emit('toggle-batches', id)"
        @toggle-node="(id) => emit('toggle-node', id)"
        @replay-step="(id) => emit('replay-step', id)"
        @play-step="(id) => emit('play-step', id)"
      />
    </div>
  </div>
</template>
