<script setup lang="ts">
import { RouterLink, RouterView } from 'vue-router'
</script>

<template>
  <div class="app">
    <h1>🦑 squishy</h1>
    <nav class="nav">
      <RouterLink to="/">Projects</RouterLink>
    </nav>
    <RouterView />
  </div>
</template>
