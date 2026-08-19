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
      // connectClient.ts's transport uses baseUrl "/", so every RPC call
      // goes to /agentfleet.v1.<Service>/<Method> directly — not under
      // /api. Without this, the dev server 404s every call and
      // connect-web surfaces it as a generic "unimplemented" error.
      '^/agentfleet\\.v1\\.\\w+/': 'http://localhost:8080',
      // The OIDC login round-trip (infra-bootstrap ADR-0041). Without this
      // the dev server 404s /auth/login and /auth/callback, so the auth path
      // becomes the one path /dashboard-e2e — the harness CLAUDE.md says to
      // run after touching the create, decision or warm path — can never
      // exercise. Local stacks still set FLEET_AUTH_DISABLED=1; this is what
      // makes testing WITH the gate possible at all.
      '/auth/': 'http://localhost:8080',
    },
  },
})
