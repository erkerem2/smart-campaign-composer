import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// The Go API (backend/cmd/server) listens on :8080. In development every
// /api and /healthz request is proxied there, so the browser talks to one
// origin and CORS never comes into play. Override with VITE_API_TARGET.
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const target = env.VITE_API_TARGET || 'http://localhost:8080'

  const proxy = {
    target,
    changeOrigin: true,
    // SSE: keep the connection open and never buffer.
    timeout: 0,
    proxyTimeout: 0,
  }

  return {
    plugins: [vue(), tailwindcss()],
    server: {
      port: 5173,
      strictPort: true,
      proxy: { '/api': proxy, '/healthz': proxy },
    },
    preview: {
      port: 4173,
      proxy: { '/api': proxy, '/healthz': proxy },
    },
    test: {
      environment: 'jsdom',
      include: ['tests/**/*.test.js'],
      restoreMocks: true,
    },
  }
})
