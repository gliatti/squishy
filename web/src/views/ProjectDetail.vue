<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, type Instance, type Project } from '../api/client'

const route = useRoute()
const projectID = route.params.id as string

const project = ref<Project | null>(null)
const instances = ref<Instance[]>([])
const err = ref('')

const editing = ref(false)
const editName = ref('')
const editDesc = ref('')
const saving = ref(false)

async function refresh() {
  err.value = ''
  try {
    project.value = await api.getProject(projectID)
    instances.value = (await api.listInstances(projectID)).instances || []
  } catch (e: any) {
    err.value = e.message
  }
}

function startEdit() {
  if (!project.value) return
  editName.value = project.value.name
  editDesc.value = project.value.description || ''
  editing.value = true
}

function cancelEdit() {
  editing.value = false
  err.value = ''
}

async function saveEdit() {
  if (!editName.value.trim()) { err.value = 'name required'; return }
  saving.value = true
  err.value = ''
  try {
    project.value = await api.updateProject(projectID, {
      name: editName.value,
      description: editDesc.value,
    })
    editing.value = false
  } catch (e: any) {
    err.value = e.message
  } finally {
    saving.value = false
  }
}

async function removeInstance(id: string) {
  if (!confirm('Delete this instance and all its migrations?')) return
  await api.deleteInstance(id)
  refresh()
}

onMounted(refresh)
</script>

<template>
  <section>
    <p><router-link to="/">← All projects</router-link></p>

    <div v-if="project && !editing">
      <h2>{{ project.name }} <span class="pill">{{ project.slug }}</span></h2>
      <p v-if="project.description">{{ project.description }}</p>
      <p>
        <button class="secondary" @click="startEdit">✎ Edit project</button>
      </p>
    </div>

    <div v-else-if="project && editing" class="card">
      <h3>Edit project</h3>
      <label>Name</label>
      <input v-model="editName" />
      <label>Description</label>
      <input v-model="editDesc" />
      <p style="opacity:.7; font-size:0.85rem;">
        slug stays <code>{{ project.slug }}</code> (immutable to keep URLs stable).
      </p>
      <p style="margin-top:1rem">
        <button @click="saveEdit" :disabled="saving">{{ saving ? 'Saving…' : 'Save' }}</button>
        &nbsp;<button class="secondary" @click="cancelEdit" :disabled="saving">Cancel</button>
      </p>
    </div>

    <p v-if="err" class="err">{{ err }}</p>

    <div class="card">
      <h3>Instances</h3>
      <p style="opacity:.7">An instance bundles a source endpoint (Oracle/MySQL/MariaDB super-user) with its target PG endpoint and a mapping strategy. squishy auto-discovers the non-system schemas and creates a draft migration per schema.</p>
      <p>
        <router-link :to="`/projects/${projectID}/instances/new`">
          <button>+ Add instance</button>
        </router-link>
      </p>
      <div v-if="!instances.length" style="opacity:.7">No instances yet.</div>
      <div v-for="i in instances" :key="i.id" style="border-top:1px solid #eee; padding:0.6rem 0;">
        <strong>{{ i.name }}</strong>
        <span class="pill">{{ i.kind }}</span>
        <span class="pill">{{ i.target_strategy }}</span>
        <p style="margin:0.3rem 0; opacity:.8;">
          source: <code>{{ i.username }}@{{ i.host }}:{{ i.port }}/{{ i.database }}</code><br/>
          target: <code>{{ i.target_username }}@{{ i.target_host }}:{{ i.target_port }}/{{ i.target_database }}</code>
        </p>
        <p style="margin:0.3rem 0;">
          <router-link :to="`/instances/${i.id}`">Open</router-link>
          &nbsp;·&nbsp;
          <a href="#" @click.prevent="removeInstance(i.id)">delete</a>
        </p>
      </div>
    </div>
  </section>
</template>
