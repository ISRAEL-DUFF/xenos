import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: { proxy: { "/v1": "http://localhost:8080" } },
  // noVNC uses top-level await, which needs an ES2022 target.
  build: { outDir: "dist", emptyOutDir: true, target: "es2022" },
});
