import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  server: {
    host: '0.0.0.0',
    port: 5173,
    // Docker Desktop on Windows/macOS doesn't deliver native fs events across
    // the bind-mount boundary, so Vite's chokidar watcher never sees host
    // edits. usePolling is the only reliable HMR strategy here.
    watch: { usePolling: true, interval: 300 },
    proxy: {
      '/api': {
        target: process.env.VITE_API_BASE || 'http://api:8080',
        changeOrigin: true,
      },
    },
  },
})
