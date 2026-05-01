<script setup lang="ts">
import type { Batch } from '../../api/client'

const props = defineProps<{ batch: Batch }>()

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

function rangeLabel(b: Batch): string {
  if (b.range_kind === 'full') return 'full'
  const low = b.range_low == null ? '−∞' : JSON.stringify(b.range_low)
  const high = b.range_high == null ? '+∞' : JSON.stringify(b.range_high)
  return `${low} → ${high}`
}
</script>

<template>
  <div class="batch-card" :class="props.batch.status">
    <div class="batch-row">
      <span class="batch-seq">#{{ props.batch.seq }}</span>
      <span :class="pillClass(props.batch.status)">{{ props.batch.status }}</span>
      <span class="batch-rows">{{ props.batch.row_count }}/{{ props.batch.row_count_est }}</span>
    </div>
    <div class="batch-range"><code>{{ rangeLabel(props.batch) }}</code></div>
    <div class="progress" v-if="props.batch.row_count_est > 0">
      <span :style="{ width: pct(props.batch.row_count, props.batch.row_count_est) + '%' }"></span>
    </div>
    <div v-if="props.batch.error" class="batch-error" :title="props.batch.error">{{ props.batch.error }}</div>
  </div>
</template>
