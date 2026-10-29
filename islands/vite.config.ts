import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [svelte()], // compiler options: svelte.config.js
  // The generated AgentService code lives in dashboard/src/gen; its imports
  // resolve to this package's copy, not the dashboard's node_modules.
  resolve: { dedupe: ["@bufbuild/protobuf", "@connectrpc/connect"] },
  build: {
    // One stable entry, served no-cache by core; mermaid is the only lazy
    // chunk, content-hashed and immutable (docs/adr/0068 §3).
    lib: {
      entry: "src/main.ts",
      formats: ["es"],
      fileName: () => "fleet-agent.js",
    },
    outDir: "dist",
    // The entry's static imports (a DOMPurify chunk it shares with mermaid)
    // load with it, so a fresh page always gets a matching set. Only lazy
    // chunks can go missing on a page left open across a release, and those
    // are mermaid's: diagram.ts then shows the source.
    target: "es2022",
  },
});
