import { defineConfig, devices } from "@playwright/test";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";

// Browser benchmarks (mise run bench:web): not part of CI's e2e suite.
// BENCH_RUNS sets the repetitions, BENCH_QUERY extra page parameters (e.g.
// engine=interp), BENCH_LABEL the name of the results file in bench-results/.
export default defineConfig({
  testDir: ".",
  timeout: 1_800_000,
  workers: 1,
  reporter: "list",
  use: { baseURL: "http://localhost:4175/bootlet/", ...devices["Desktop Chrome"] },
  webServer: {
    command: "pnpm exec vite preview --port 4175 --strictPort --base /bootlet/",
    cwd: dirname(dirname(fileURLToPath(import.meta.url))),
    url: "http://localhost:4175/bootlet/",
    reuseExistingServer: false,
  },
});
