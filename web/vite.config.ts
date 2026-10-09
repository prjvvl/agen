import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build is embedded in the Hub (platform/internal/ui) and served at "/".
// `npm run dev` proxies the API to a local Hub (agen up).
export default defineConfig({
  plugins: [react()],
  build: { outDir: "../platform/internal/ui/dist", emptyOutDir: true },
  server: {
    proxy: {
      "/agen.v1.HubService": process.env.AGEN_HUB ?? "http://127.0.0.1:7070",
    },
  },
});
