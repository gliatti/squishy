<script setup lang="ts">
import { computed } from 'vue'
import type { Step, Batch } from '../../api/client'
import LevelLane from './LevelLane.vue'

const props = defineProps<{
  steps: Step[]
  batchesByStep: Map<string, Batch[]>
  expandedSteps: Set<string>
}>()
const emit = defineEmits<{
  (e: 'toggle-batches', stepId: string): void
  (e: 'play', level: number): void
  (e: 'replay', level: number): void
  (e: 'replay-step', stepId: string): void
  (e: 'play-step', stepId: string): void
}>()

const byLevel = computed(() => {
  const m = new Map<number, Step[]>()
  for (const s of props.steps) {
    const lvl = s.level ?? 0
    if (!m.has(lvl)) m.set(lvl, [])
    m.get(lvl)!.push(s)
  }
  for (const arr of m.values()) {
    arr.sort((a, b) => (a.priority - b.priority) || (a.seq - b.seq))
  }
  return [...m.entries()].sort((a, b) => a[0] - b[0])
})
</script>

<template>
  <div class="levels-grid">
    <LevelLane
      v-for="[lvl, steps] in byLevel"
      :key="lvl"
      :level="lvl"
      :steps="steps"
      :batches-by-step="props.batchesByStep"
      :expanded-steps="props.expandedSteps"
      @toggle-batches="(id) => emit('toggle-batches', id)"
      @play="(l) => emit('play', l)"
      @replay="(l) => emit('replay', l)"
      @replay-step="(id) => emit('replay-step', id)"
      @play-step="(id) => emit('play-step', id)"
    />
  </div>
</template>
