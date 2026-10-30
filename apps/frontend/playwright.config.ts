import { defineConfig, devices } from "@playwright/test";

// Serves the production build from a sub-path, like GitHub Pages does.
export default defineConfig({
  testDir: "e2e",
  timeout: 180_000,
  // CI runs this alongside the Go tests on a small runner; give the guest room.
  expect: { timeout: 30_000 },
  use: { baseURL: "http://localhost:4174/bootlet/", ...devices["Desktop Chrome"] },
  webServer: {
    command: "pnpm exec vite preview --port 4174 --strictPort --base /bootlet/",
    url: "http://localhost:4174/bootlet/",
    reuseExistingServer: !process.env.CI,
  },
});
