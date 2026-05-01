<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, type Migration } from '../api/client'
import DDLTab from '../components/migration/DDLTab.vue'
import WarningsTab from '../components/migration/WarningsTab.vue'
import ExecutionTab from '../components/migration/ExecutionTab.vue'

type Tab = 'ddl' | 'warnings' | 'exec'

const route = useRoute()
const migrationID = route.params.mid as string

const migration = ref<Migration | null>(null)
const err = ref('')
const TAB_KEY = 'squishy.migrationDetail.tab'
const tab = ref<Tab>(((): Tab => {
  const v = localStorage.getItem(TAB_KEY)
  if (v === 'warnings' || v === 'exec' || v === 'ddl') return v
  return 'ddl'
})())

function setTab(t: Tab) {
  tab.value = t
  localStorage.setItem(TAB_KEY, t)
}

async function refresh() {
  err.value = ''
  try {
    migration.value = await api.getMigration(migrationID)
  } catch (e: any) {
    err.value = e.message
  }
}

const breadcrumbInstance = computed(() => migration.value?.instance_id || '')

onMounted(refresh)
</script>

<template>
  <section>
    <p>
      <router-link v-if="breadcrumbInstance" :to="`/instances/${breadcrumbInstance}`">
        ← Back to instance
      </router-link>
    </p>
    <h2 v-if="migration">
      <code>{{ migration.source_schema_name }}</code>
      → <code>{{ migration.target_db_name }}.{{ migration.target_schema_name }}</code>
      <span class="pill">{{ migration.status }}</span>
    </h2>
    <p v-if="err" class="err">{{ err }}</p>

    <div class="card">
      <nav class="tabs" role="tablist">
        <button role="tab" :class="{ active: tab === 'ddl' }" :aria-selected="tab === 'ddl'" @click="setTab('ddl')">DDL</button>
        <button role="tab" :class="{ active: tab === 'warnings' }" :aria-selected="tab === 'warnings'" @click="setTab('warnings')">Warnings &amp; blockers</button>
        <button role="tab" :class="{ active: tab === 'exec' }" :aria-selected="tab === 'exec'" @click="setTab('exec')">Execution</button>
      </nav>

      <DDLTab v-if="tab === 'ddl' && migration"
        :migration-id="migration.id"
        :status="migration.status"
        @planned="refresh" />

      <WarningsTab v-if="tab === 'warnings' && migration"
        :migration-id="migration.id"
        :status="migration.status" />

      <ExecutionTab v-if="tab === 'exec' && migration"
        :migration-id="migration.id"
        :status="migration.status" />
    </div>
  </section>
</template>
