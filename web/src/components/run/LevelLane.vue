<script setup lang="ts">
import { computed } from 'vue'
import type { Step, Batch } from '../../api/client'
import StepCard from './StepCard.vue'

const props = defineProps<{
  level: number
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

const stats = computed(() => {
  const done = props.steps.filter(s => s.status === 'succeeded').length
  const running = props.steps.filter(s => s.status === 'running').length
  const failed = props.steps.filter(s => s.status === 'failed' || s.status === 'cancelled').length
  const locked = props.steps.some(s => !s.unlocked)
  const allDone = done === props.steps.length && props.steps.length > 0
  return { done, running, failed, total: props.steps.length, locked, allDone }
})

// Show "Play" whenever the level isn't fully succeeded — covers locked, failed,
// cancelled, stuck-in-retry, or simply not-yet-started. Always gives the user
// an escape hatch to force-kick a stalled step.
const canPlay = computed(() => !stats.value.allDone)

// Replay is always available — it resets this level and every downstream level
// back to pending. Useful post-success (re-run the whole tail) or post-failure.
const canReplay = computed(() => props.steps.length > 0)
</script>

<template>
  <div class="level-lane">
    <header>
      <span class="level-num">Niveau {{ props.level }}</span>
      <span class="level-stats">
        {{ stats.done }}/{{ stats.total }}<template v-if="stats.running > 0"> · {{ stats.running }} ⟳</template><template v-if="stats.failed > 0"> · {{ stats.failed }} ✕</template>
      </span>
    </header>
    <div class="level-actions" v-if="canPlay || canReplay">
      <button
        v-if="canPlay"
        class="lane-btn play"
        :title="stats.locked ? 'Déverrouiller et lancer ce niveau' : 'Relancer les steps échoués'"
        @click="emit('play', props.level)"
      >▶ Play</button>
      <button
        v-if="canReplay"
        class="lane-btn replay"
        title="Réinitialiser et relancer ce niveau"
        @click="emit('replay', props.level)"
      >⟲ Replay</button>
    </div>
    <StepCard
      v-for="s in props.steps"
      :key="s.id"
      :step="s"
      :batches="props.batchesByStep.get(s.id)"
      :expanded="props.expandedSteps.has(s.id)"
      @toggle-batches="(id) => emit('toggle-batches', id)"
      @replay-step="(id) => emit('replay-step', id)"
      @play-step="(id) => emit('play-step', id)"
    />
  </div>
</template>
