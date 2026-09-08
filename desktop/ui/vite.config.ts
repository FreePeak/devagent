import { defineConfig } from "vite";

// Static TS webview app — no framework preset. The packaged app serves the
// built dist through Tauri's asset protocol (tauri.conf.json frontendDist);
// `npm run dev` is for iterating in a plain browser (pass ?daemon=&token=).
export default defineConfig({
  clearScreen: false,
  server: { port: 5188, strictPort: true },
  build: { target: "es2022", outDir: "dist", emptyOutDir: true },
});
