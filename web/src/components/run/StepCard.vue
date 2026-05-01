<script setup lang="ts">
import { computed } from 'vue'
import type { Step, Batch } from '../../api/client'
import BatchCard from './BatchCard.vue'

const props = defineProps<{
  step: Step
  batches?: Batch[]
  expanded?: boolean
  compact?: boolean
}>()
const emit = defineEmits<{
  (e: 'toggle-batches', stepId: string): void
  (e: 'play-step', stepId: string): void
  (e: 'replay-step', stepId: string): void
}>()

const hasBatches = computed(() => props.step.kind === 'copy_table')

const kindLabel: Record<string, string> = {
  inspect: 'INSPECT',
  create_ddl: 'DDL',
  copy_table: 'COPY',
  copy_batch: 'BATCH',
  create_index: 'INDEX',
  create_fk: 'FK',
  create_routine: 'ROUTINE',
  validate: 'VALIDATE',
}

function pillClass(status: string): string {
  if (status === 'succeeded') return 'pill ok'
  if (status === 'running') return 'pill run'
  if (status === 'failed' || status === 'cancelled' || status === 'dead') return 'pill fail'
  return 'pill'
}

function pct(done: number, total: number): number {
  if (!total) return 0
  return Math.min(100, Math.round((done / total) * 100))
}

function toggle() {
  if (!hasBatches.value) return
  emit('toggle-batches', props.step.id)
}
</script>

<template>
  <article class="step-card" :class="[props.step.status, { compact: props.compact }]">
    <header class="step-head">
      <button
        v-if="hasBatches"
        class="chevron"
        :aria-expanded="!!props.expanded"
        :title="props.expanded ? 'Replier' : 'Déplier les batches'"
        @click="toggle"
      >{{ props.expanded ? '▾' : '▸' }}</button>
      <span class="kind-tag">{{ kindLabel[props.step.kind] || props.step.kind }}</span>
      <code class="target" :title="props.step.target || ''">{{ props.step.target || '—' }}</code>
      <span :class="pillClass(props.step.status)">{{ props.step.status }}</span>
      <button
        v-if="props.step.status === 'failed' || props.step.status === 'cancelled' || !props.step.unlocked"
        class="step-play-btn"
        title="Relancer ce step uniquement"
        @click.stop="emit('play-step', props.step.id)"
      >▶</button>
      <button
        v-if="props.step.status !== 'succeeded'"
        class="step-replay-btn"
        title="Relancer ce step et ses descendants"
        @click.stop="emit('replay-step', props.step.id)"
      >⟲</button>
    </header>

    <div class="step-meta">
      <span v-if="props.step.rows_total > 0">{{ props.step.rows_done }} / {{ props.step.rows_total }} rows</span>
      <span
        v-if="props.step.job_max_attempts > 0 && props.step.job_attempts > 0"
        class="attempts"
        :class="{ retrying: props.step.job_attempts > 1 && props.step.status !== 'succeeded' }"
        :title="props.step.job_attempts > 1 ? `Tentative ${props.step.job_attempts}/${props.step.job_max_attempts} après erreur` : `Tentative ${props.step.job_attempts}/${props.step.job_max_attempts}`"
      >
        <template v-if="props.step.job_attempts > 1 && props.step.status !== 'succeeded'">↻ </template>tentative {{ props.step.job_attempts }}/{{ props.step.job_max_attempts }}
      </span>
    </div>

    <div class="progress" v-if="props.step.rows_total > 0">
      <span :style="{ width: pct(props.step.rows_done, props.step.rows_total) + '%' }"></span>
    </div>

    <div v-if="props.step.error" class="step-error" :title="props.step.error">{{ props.step.error }}</div>
    <div
      v-else-if="props.step.last_error && props.step.status !== 'succeeded'"
      class="step-error step-retry"
      :title="'Tentative en cours après erreur : ' + props.step.last_error"
    >↻ {{ props.step.last_error }}</div>

    <div v-if="props.expanded && hasBatches" class="batch-list">
      <template v-if="props.batches && props.batches.length">
        <BatchCard v-for="b in props.batches" :key="b.id" :batch="b" />
      </template>
      <template v-else-if="props.batches">
        <p class="muted">Aucun batch.</p>
      </template>
      <template v-else>
        <p class="muted">Chargement…</p>
      </template>
    </div>
  </article>
</template>
