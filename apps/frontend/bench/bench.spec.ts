import { expect, test, type Page } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// Repeatable browser benchmarks. "Active" MIPS counts only the time the
// worker spends inside the emulator's run(); "wall" MIPS divides by elapsed
// time, so it also falls when the guest idles or the page is busy elsewhere.

const RUNS = Number(process.env.BENCH_RUNS) || 5;
const QUERY = process.env.BENCH_QUERY ?? "";
const LABEL = process.env.BENCH_LABEL ?? "web";
const OUT = join(dirname(fileURLToPath(import.meta.url)), "../../../bench-results");

const WORKLOADS = [
  ["md5sum-16MiB", "dd if=/dev/zero bs=1M count=16 2>/dev/null | md5sum"],
  ["gzip-2MiB", "head -c 2097152 /dev/zero | tr '\\000' 'a' | gzip -1 | wc -c"],
  ["shell-loop-20k", "i=0; while [ $i -lt 20000 ]; do i=$((i+1)); done; echo $i"],
] as const;

type Stats = { totalInstructions: number; totalBusyMs: number; totalFrames: number };
type Probe = { statsNow(): Promise<Stats>; times: Record<string, number> };
type Result = { name: string; wallMs: number; activeMs: number; instructions: number; activeMips: number; wallMips: number };

const statsNow = (page: Page) => page.evaluate(() => (globalThis as unknown as { __bootlet: Probe }).__bootlet.statsNow());
const times = (page: Page) => page.evaluate(() => (globalThis as unknown as { __bootlet: Probe }).__bootlet.times);

function result(name: string, wallMs: number, a: Stats, b: Stats): Result {
  const instructions = b.totalInstructions - a.totalInstructions;
  const activeMs = b.totalBusyMs - a.totalBusyMs;
  return { name, wallMs, activeMs, instructions, activeMips: instructions / activeMs / 1000, wallMips: instructions / wallMs / 1000 };
}

const zero: Stats = { totalInstructions: 0, totalBusyMs: 0, totalFrames: 0 };
const all: Record<string, Result[]> = {};
const add = (r: Result) => (all[r.name] ??= []).push(r);

async function boot(page: Page, query: string) {
  await page.goto(`./?relay=&${query}&${QUERY}`);
  const screen = page.locator(".xterm-rows");
  await expect(screen).toContainText("root@bootlet:~#", { timeout: 600_000 });
  const s = await statsNow(page);
  const t = await times(page);
  return { screen, s, t };
}

test("benchmarks", async ({ browser }) => {
  for (let run = 0; run < RUNS; run++) {
    // A fixed instruction stream: the instruction-count clock makes every
    // boot execute the same instructions (no idle time either).
    {
      const page = await (await browser.newContext()).newPage();
      const { s, t } = await boot(page, "deterministic&splash=0");
      add(result("deterministic-boot", t.prompt - t.booting, zero, s));
      add({ ...result("page-load-to-prompt (deterministic)", t.prompt, zero, s) });
      await page.context().close();
    }
    // A normal boot, then the guest workloads.
    {
      const page = await (await browser.newContext()).newPage();
      const { screen, s, t } = await boot(page, "");
      add(result("boot", t.prompt - t.booting, zero, s));
      add(result("page-load-to-prompt", t.prompt, zero, s));
      await page.locator(".xterm").click();
      for (const [name, cmd] of WORKLOADS) {
        const marker = `__${name.replace(/\W/g, "")}${run}__`;
        const a = await statsNow(page);
        const t0 = Date.now();
        await page.keyboard.type(`${cmd}; echo ${marker.slice(0, 4)}''${marker.slice(4)}\n`);
        await expect(screen).toContainText(marker, { timeout: 600_000 });
        const wall = Date.now() - t0;
        add(result(name, wall, a, await statsNow(page)));
      }
      await page.context().close();
    }
  }

  const median = (v: number[]) => [...v].sort((x, y) => x - y)[Math.floor(v.length / 2)];
  const summary = Object.entries(all).map(([name, rs]) => ({
    name,
    wallMs: median(rs.map((r) => r.wallMs)),
    activeMs: median(rs.map((r) => r.activeMs)),
    instructions: median(rs.map((r) => r.instructions)),
    activeMips: median(rs.map((r) => r.activeMips)),
    activeMipsRange: [Math.min(...rs.map((r) => r.activeMips)), Math.max(...rs.map((r) => r.activeMips))],
    wallMips: median(rs.map((r) => r.wallMips)),
  }));
  for (const r of summary) {
    console.log(
      `${r.name.padEnd(38)} wall ${r.wallMs.toFixed(0).padStart(7)} ms  active ${r.activeMs.toFixed(0).padStart(7)} ms  ` +
        `${String(r.instructions).padStart(11)} instr  active ${r.activeMips.toFixed(1).padStart(5)} MIPS ` +
        `(${r.activeMipsRange.map((v) => v.toFixed(1)).join("-")})  wall ${r.wallMips.toFixed(1).padStart(5)} MIPS`,
    );
  }
  mkdirSync(OUT, { recursive: true });
  writeFileSync(join(OUT, `${LABEL}.json`), JSON.stringify({ runs: RUNS, query: QUERY, summary, all }, null, 2));
});
