<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, type Project } from '../api/client'

const projects = ref<Project[]>([])
const name = ref('')
const desc = ref('')
const err = ref('')
const router = useRouter()

async function refresh() {
  try { projects.value = (await api.listProjects()).projects || [] }
  catch (e: any) { err.value = e.message }
}

async function create() {
  err.value = ''
  if (!name.value.trim()) { err.value = 'name required'; return }
  try {
    const p = await api.createProject({ name: name.value, description: desc.value })
    router.push(`/projects/${p.id}`)
  } catch (e: any) { err.value = e.message }
}

async function remove(id: string) {
  if (!confirm('Delete this project?')) return
  await api.deleteProject(id)
  refresh()
}

onMounted(refresh)
</script>

<template>
  <section>
    <h2>Create a migration project</h2>
    <div class="card">
      <label>Name</label>
      <input v-model="name" placeholder="my-migration" />
      <label>Description</label>
      <input v-model="desc" placeholder="optional" />
      <p v-if="err" class="err">{{ err }}</p>
      <p style="margin-top:1rem"><button @click="create">Create →</button></p>
    </div>

    <h2>Existing projects</h2>
    <div v-if="!projects.length" class="card">No projects yet.</div>
    <div v-for="p in projects" :key="p.id" class="card">
      <strong>{{ p.name }}</strong> <span class="pill">{{ p.slug }}</span>
      <p>{{ p.description || '—' }}</p>
      <p>
        <router-link :to="`/projects/${p.id}`">Open project</router-link>
        &nbsp;·&nbsp;
        <a href="#" @click.prevent="remove(p.id)">delete</a>
      </p>
    </div>
  </section>
</template>
