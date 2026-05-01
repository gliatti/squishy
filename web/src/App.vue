<script setup lang="ts">
import { RouterLink, RouterView } from 'vue-router'
</script>

<template>
  <div class="app">
    <h1 class="brand"><img src="/logo.svg" alt="squishy" height="44" /></h1>
    <nav class="nav">
      <RouterLink to="/">Projects</RouterLink>
    </nav>
    <RouterView />
  </div>
</template>
