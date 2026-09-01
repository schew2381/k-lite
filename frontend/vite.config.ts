import path from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// base './' keeps the built bundle servable from any path — the Go server
// will embed dist/ via go:embed and serve it from /.
export default defineConfig({
  // live mode in the dev server stays same-origin: /api proxies to a local
  // klite-facade, so no CORS flag and no VITE_KLITE_API are needed
  server: {
    proxy: {
      '/api': { target: 'http://127.0.0.1:7080', changeOrigin: true },
    },
  },
  base: './',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './app'),
    },
  },
})
