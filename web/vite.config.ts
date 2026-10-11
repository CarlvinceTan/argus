import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";

// The SPA is embedded into the Go binary from internal/webui/dist, so the same
// artifact serves locally and over the network without a node runtime.
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
  },
  server: {
    host: "127.0.0.1",
    port: 5199,
    strictPort: true,
    proxy: {
      "/api": "http://127.0.0.1:8787",
      "/evidence": "http://127.0.0.1:8787",
    },
  },
});
