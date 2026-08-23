import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  build: {
    // Vendor chunk isolation (v1.9.x perf).
    //
    // The react/react-dom/scheduler runtime is imported by every route
    // and never changes between deploys. Splitting it into a dedicated
    // content-hashed chunk means after a redeploy the browser re-fetches
    // ONLY the changed app chunk(s) — the react vendor chunk stays a
    // long-lived immutable cache hit (nginx already serves it with
    // `public, immutable, max-age=30d`). Without this, a change to one
    // page rewrites the single app chunk's hash and forces the whole
    // bundle to re-download on every release.
    //
    // We deliberately do NOT split axios here: axios is shared by the
    // app shell and stays in the entry chunk. hls.js IS split into its
    // own chunk (vendor-hls) so the Cameras route chunk does not exceed
    // the 500kB warning threshold and hls.js ships as an independent,
    // long-cached unit that only changes when the hls.js package version
    // is bumped.
    // vendor-hls (hls.js) is intentionally a single large, immutable,
    // long-cached chunk that is OFF the login/dashboard critical path —
    // it only loads when a camera is opened. Raising the warning threshold
    // (default 500 kB) to cover that stable vendor chunk stops the noisy
    // "larger than 500 kB" warning without hiding genuinely oversized app
    // code (our largest real route chunk is Cameras at ~53 kB). Set just
    // above hls.js's 524 kB so future app-chunk regressions still warn.
    chunkSizeWarningLimit: 600,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (
            id.includes("/node_modules/react/") ||
            id.includes("/node_modules/react-dom/") ||
            id.includes("/node_modules/scheduler/")
          ) {
            return "vendor-react";
          }
          // hls.js: isolate into its own chunk (only ever reached from
          // the Cameras route). Keep it OUT of the app shell so the
          // dashboard/login path never downloads it.
          if (id.includes("/node_modules/hls.js/")) {
            return "vendor-hls";
          }
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      // WebSocket upgrade must be listed BEFORE the catch-all /api proxy
      // so Vite matches the longer path first and enables ws:true.
      "/api/v1/ws": {
        target: "http://localhost:8080",
        changeOrigin: true,
        ws: true,
      },
      // REST API proxy -> backend on :8080
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});
