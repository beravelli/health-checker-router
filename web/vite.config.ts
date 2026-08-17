import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The dev server proxies the API so the browser sees one origin and the
// session cookie is sent without CORS being involved at all.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
  build: { outDir: 'dist', sourcemap: true },
})
