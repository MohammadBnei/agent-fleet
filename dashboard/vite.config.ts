import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // Same-origin in production (fleet-core serves this build's dist/
  // directly), but the dev server needs to proxy /api and /mcp-adjacent
  // dashboard calls to a locally-running fleet-core instance.
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
