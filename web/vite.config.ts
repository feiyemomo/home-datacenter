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
