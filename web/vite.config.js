import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// base './' keeps asset URLs relative so the build works when served from the
// embedded Go binary at any mount point. The dev proxy forwards /api to the Go
// server (run `go run ./cmd/server` alongside `npm run dev`).
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: 'dist',
    // Keep dist/.gitkeep (committed so `//go:embed all:dist` compiles on a fresh
    // clone). emptyOutDir would delete it, so we clear stale assets ourselves.
    emptyOutDir: false,
  },
  server: {
    port: 5273,
    proxy: {
      '/api': {
        target: 'http://localhost:6185',
        changeOrigin: true,
      },
    },
  },
})
