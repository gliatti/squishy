<script setup lang="ts">
import { computed } from 'vue'
import type { Step, Batch } from '../../api/client'
import StepCard from './StepCard.vue'

const props = defineProps<{
  step: Step
  childrenMap: Map<string, Step[]>
  batchesByStep: Map<string, Batch[]>
  expandedSteps: Set<string>
  expandedNodes: Set<string>
}>()
const emit = defineEmits<{
  (e: 'toggle-batches', stepId: string): void
  (e: 'toggle-node', stepId: string): void
  (e: 'replay-step', stepId: string): void
  (e: 'play-step', stepId: string): void
}>()

const children = computed(() => props.childrenMap.get(props.step.id) ?? [])
const open = computed(() => props.expandedNodes.has(props.step.id))

function toggleNode() {
  emit('toggle-node', props.step.id)
}
</script>

<template>
  <div class="tree-node">
    <div class="tree-row">
      <button
        class="chevron"
        v-if="children.length > 0"
        :aria-expanded="open"
        :title="open ? 'Replier' : 'Déplier'"
        @click="toggleNode"
      >{{ open ? '▾' : '▸' }}</button>
      <span v-else class="chevron placeholder"></span>
      <StepCard
        class="tree-step"
        :step="props.step"
        :batches="props.batchesByStep.get(props.step.id)"
        :expanded="props.expandedSteps.has(props.step.id)"
        compact
        @toggle-batches="(id) => emit('toggle-batches', id)"
        @replay-step="(id) => emit('replay-step', id)"
        @play-step="(id) => emit('play-step', id)"
      />
    </div>
    <div class="children" v-if="open && children.length > 0">
      <TreeNode
        v-for="c in children"
        :key="c.id"
        :step="c"
        :children-map="props.childrenMap"
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
