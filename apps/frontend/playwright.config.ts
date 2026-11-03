import { defineConfig, devices } from "@playwright/test";

// Serves the production build from a sub-path, like GitHub Pages does, next
// to a local Wisp relay for the networking tests.
export default defineConfig({
  testDir: "e2e",
  timeout: 240_000,
  // CI runs this alongside the Go tests on a small runner; give the guest room.
  expect: { timeout: 30_000 },
  use: { baseURL: "http://localhost:4174/bootlet/", ...devices["Desktop Chrome"] },
  webServer: [
    {
      command: "pnpm exec vite preview --port 4174 --strictPort --base /bootlet/",
      url: "http://localhost:4174/bootlet/",
      reuseExistingServer: !process.env.CI,
    },
    {
      // -allow-private and -ports all let the guest reach the preview server
      // through the gateway address; never run a public relay like this.
      command: "go run ../relay -addr 127.0.0.1:18089 -allow-private -ports all",
      url: "http://127.0.0.1:18089/healthz",
      reuseExistingServer: !process.env.CI,
    },
  ],
});
