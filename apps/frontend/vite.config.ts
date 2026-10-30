import { defineConfig } from "vite";

export default defineConfig({
  // Relative asset URLs so the build works from any sub-path (GitHub Pages).
  base: "./",
  worker: { format: "es" },
  build: { target: "es2022" },
});
