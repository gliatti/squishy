<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, type Instance, type Migration } from '../api/client'

const route = useRoute()
const instanceID = route.params.iid as string

const instance = ref<Instance | null>(null)
const migrations = ref<Migration[]>([])
const err = ref('')
const sourceTestResult = ref('')
const targetTestResult = ref('')
const rediscovering = ref(false)

async function refresh() {
  err.value = ''
  try {
    const res = await api.getInstance(instanceID)
    instance.value = res.instance
    migrations.value = res.migrations || []
  } catch (e: any) {
    err.value = e.message
  }
}

async function rediscover() {
  rediscovering.value = true
  err.value = ''
  try {
    const res = await api.rediscoverInstance(instanceID)
    migrations.value = res.migrations || []
    if (res.discover_error) err.value = `Discovery failed: ${res.discover_error}`
  } catch (e: any) {
    err.value = e.message
  } finally {
    rediscovering.value = false
  }
}

async function testSource() {
  sourceTestResult.value = 'Testing...'
  try {
    const r = await api.testInstanceConnection(instanceID)
    sourceTestResult.value = r.ok ? `OK (${r.version})` : r.message
  } catch (e: any) {
    sourceTestResult.value = e.message
  }
}

async function testTarget() {
  targetTestResult.value = 'Testing...'
  try {
    const r = await api.testInstanceTargetConnection(instanceID)
    targetTestResult.value = r.ok ? `OK (${r.version})` : r.message
  } catch (e: any) {
    targetTestResult.value = e.message
  }
}

function targetLabel(m: Migration): string {
  return `${m.target_db_name}.${m.target_schema_name}`
}

function statusPill(s: string): string {
  if (s === 'planned') return 'pill ok'
  if (s === 'draft') return 'pill'
  return 'pill'
}

function runPill(s: string): string {
  if (s === 'succeeded') return 'pill ok'
  if (s === 'pending' || s === 'running') return 'pill run'
  if (s === 'failed' || s === 'cancelled') return 'pill fail'
  return 'pill'
}

onMounted(refresh)
</script>

<template>
  <section>
    <p>
      <router-link v-if="instance" :to="`/projects/${instance.project_id}`">← Back to project</router-link>
    </p>
    <h2 v-if="instance">
      {{ instance.name }}
      <span class="pill">{{ instance.kind }}</span>
      <span class="pill">{{ instance.target_strategy }}</span>
      &nbsp;
      <router-link :to="`/instances/${instanceID}/edit`" style="font-size:0.85rem;">✎ Edit</router-link>
    </h2>
    <p v-if="instance" style="opacity:.8">
      source: <code>{{ instance.username }}@{{ instance.host }}:{{ instance.port }}/{{ instance.database }}</code><br/>
      target: <code>{{ instance.target_username }}@{{ instance.target_host }}:{{ instance.target_port }}/{{ instance.target_database }}</code>
      <span v-if="instance.target_strategy === 'dedicated_schema'" style="opacity:.7">
        &nbsp;· schemas land in <code>{{ instance.target_database }}</code>
      </span>
    </p>
    <p v-if="err" class="err">{{ err }}</p>

    <div class="card">
      <p>
        <button class="secondary" @click="testSource">Test source</button>
        &nbsp;<span>{{ sourceTestResult }}</span>
      </p>
      <p>
        <button class="secondary" @click="testTarget">Test target</button>
        &nbsp;<span>{{ targetTestResult }}</span>
      </p>
      <p>
        <button class="secondary" @click="rediscover" :disabled="rediscovering">
          {{ rediscovering ? 'Rediscovering…' : 'Rediscover schemas' }}
        </button>
      </p>
    </div>

    <div class="card">
      <h3>Migrations ({{ migrations.length }})</h3>
      <p v-if="!migrations.length" style="opacity:.7">
        No migrations yet — click "Rediscover schemas" if the discovery step failed at instance creation.
      </p>
      <table v-else style="width:100%; border-collapse:collapse;">
        <thead>
          <tr style="text-align:left; border-bottom:1px solid #ddd;">
            <th style="padding:.4rem;">Source schema</th>
            <th style="padding:.4rem;">Target</th>
            <th style="padding:.4rem;">Plan</th>
            <th style="padding:.4rem;">Last run</th>
            <th style="padding:.4rem;"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="m in migrations" :key="m.id" style="border-bottom:1px solid #eee;">
            <td style="padding:.4rem;"><code>{{ m.source_schema_name }}</code></td>
            <td style="padding:.4rem;"><code>{{ targetLabel(m) }}</code></td>
            <td style="padding:.4rem;"><span :class="statusPill(m.status)">{{ m.status }}</span></td>
            <td style="padding:.4rem;">
              <router-link
                v-if="m.latest_run_id && m.latest_run_status"
                :to="`/runs/${m.latest_run_id}`"
                style="text-decoration:none">
                <span :class="runPill(m.latest_run_status)">{{ m.latest_run_status }}</span>
              </router-link>
              <span v-else style="opacity:.5">—</span>
            </td>
            <td style="padding:.4rem;">
              <router-link :to="`/migrations/${m.id}`">Open →</router-link>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
