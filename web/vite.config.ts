import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// In dev the Go API (docker compose app service or `make run-server`)
// listens on :8080; the SPA proxies API paths there so cookies stay
// same-origin, matching the production Caddy setup (docs/09-deployment.md).
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/healthz': 'http://localhost:8080',
      '/api': 'http://localhost:8080',
    },
  },
})
