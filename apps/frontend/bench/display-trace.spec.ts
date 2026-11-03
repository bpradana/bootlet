import { expect, test, type Page } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// Display-pipeline trace: where do frames get lost between the guest and the
// screen? Boots with ?trace=display (the emulator records every flush, the
// worker stamps a present counter in the bottom-right corner of the display)
// and captures what Chrome actually composites with a CDP screencast. It runs
// fbtick (stamped guest frames at a known rate) and Doom with forward held,
// and reports, per workload:
//   device:     flushes that changed something, complete/torn fbtick frames
//   worker:     presents per second and their spacing
//   composited: distinct present counters (and fbtick frames) in captures
// The screencast drops frames when Chrome is busy, so composited counts are
// lower bounds. Run headed; headless Chrome composites differently:
//
//   pnpm exec playwright test -c bench display-trace --headed
//   TRACE_QUERY="display=simplefb", "offscreen=1" (worker canvas, WebGPU) or
//   "renderer=2d" (worker canvas, 2D) for the other rows.

const QUERY = process.env.TRACE_QUERY ?? "";
const LABEL = process.env.TRACE_LABEL ?? (QUERY.replace(/\W+/g, "-") || "default");
const OUT = join(dirname(fileURLToPath(import.meta.url)), "../../../bench-results");
const W = 640, H = 480, MARK_W = 128, MARK_H = 16; // keep in sync with worker.ts

type Trace = { device: (Record<string, number> & { records: Record<string, number> }) | null; presents: number[][] };
type Probe = { stats?: { display: { device: string; renderer: string } }; traces: { at: number; trace: Trace }[] };
type Capture = { at: number; data: string };

const probe = (page: Page) =>
  page.evaluate(() => {
    const p = (globalThis as unknown as { __bootlet: Probe }).__bootlet;
    return { display: p.stats?.display, n: p.traces.length };
  });
const traces = (page: Page, from: number, to: number) =>
  page.evaluate(([a, b]) => (globalThis as unknown as { __bootlet: Probe }).__bootlet.traces.slice(a, b), [from, to]);

// decode reads, in the page, the present counter and the fbtick stamps from
// each captured JPEG. box: the display canvas in CSS pixels; the capture is
// at scale pixels per CSS pixel.
async function decode(page: Page, caps: Capture[], box: { x: number; y: number; width: number; height: number }) {
  return page.evaluate(
    async ({ caps, box, W, H, MARK_W, MARK_H }) => {
      const out: { at: number; seq: number; top: number; bottom: number }[] = [];
      for (const c of caps) {
        const blob = await (await fetch(`data:image/jpeg;base64,${c.data}`)).blob();
        const bmp = await createImageBitmap(blob);
        const cv = new OffscreenCanvas(bmp.width, bmp.height);
        const ctx = cv.getContext("2d")!;
        ctx.drawImage(bmp, 0, 0);
        const scale = bmp.width / innerWidth;
        // The canvas element letterboxes the screen (object-fit: contain).
        const k = Math.min(box.width / W, box.height / H);
        const x0 = box.x + (box.width - W * k) / 2, y0 = box.y + (box.height - H * k) / 2;
        const px = (gx: number, gy: number) => {
          const d = ctx.getImageData(Math.round((x0 + gx * k) * scale), Math.round((y0 + gy * k) * scale), 1, 1).data;
          return (d[0] + d[1] + d[2]) / 3;
        };
        let seq = 0;
        for (let i = 0; i < MARK_W / 16; i++) if (px(W - MARK_W + 16 * i + 8, H - MARK_H / 2) > 128) seq |= 1 << i;
        const stamp = (gy: number) => {
          let v = 0;
          for (let i = 0; i < 16; i++) {
            const g = px(40 * i + 20, gy);
            if (g >= 192) v |= 1 << i;
            else if (g > 64) return -1;
          }
          const s = (v >> 8) & 15;
          return (v & 0xff) === 0xa5 && v >> 12 === (s ^ 15) ? s : -1;
        };
        out.push({ at: c.at, seq, top: stamp(4), bottom: stamp(396) });
      }
      return out;
    },
    { caps, box, W, H, MARK_W, MARK_H },
  );
}

async function measure(page: Page, name: string, secs: number, start: () => Promise<void>) {
  const cdp = await page.context().newCDPSession(page);
  const caps: Capture[] = [];
  cdp.on("Page.screencastFrame", (f) => {
    caps.push({ at: f.metadata.timestamp ?? Date.now() / 1000, data: f.data });
    void cdp.send("Page.screencastFrameAck", { sessionId: f.sessionId });
  });
  const n0 = (await probe(page)).n;
  await start();
  const t0 = Date.now();
  await cdp.send("Page.startScreencast", { format: "jpeg", quality: 90, everyNthFrame: 1 });
  await page.waitForTimeout(secs * 1000);
  await cdp.send("Page.stopScreencast");
  const t1 = Date.now();
  const n1 = (await probe(page)).n;
  const chunks = await traces(page, n0, n1 + 1);
  const box = (await page.locator("#display").boundingBox())!;
  const seen = await decode(page, caps, box);
  if (process.env.TRACE_DUMP) {
    mkdirSync(OUT, { recursive: true });
    caps.slice(0, 40).forEach((c, i) => i % 10 === 0 && writeFileSync(join(OUT, `capture-${name}-${i}.jpg`), Buffer.from(c.data, "base64")));
    console.log("box", JSON.stringify(box), "seen", JSON.stringify(seen.slice(0, 20)));
  }
  await cdp.detach();

  // Device counters are cumulative since tracing started.
  const dev = chunks.map((c) => c.trace.device).filter((d) => d) as NonNullable<Trace["device"]>[];
  const delta = (k: string) => (dev.length > 1 ? dev[dev.length - 1][k] - dev[0][k] : 0);
  const devSecs = dev.length > 1 ? (dev[dev.length - 1].sinceMs - dev[0].sinceMs) / 1000 : 1;
  const presents = chunks.flatMap((c) => c.trace.presents);
  const gaps = presents.map((p) => p[1]).filter((g) => g > 0).sort((a, b) => a - b);
  const pct = (q: number) => gaps[Math.min(gaps.length - 1, Math.floor(q * gaps.length))] ?? 0;
  const pSecs = presents.length > 1 ? (presents[presents.length - 1][0] - presents[0][0]) / 1000 : 1;
  const span = (t1 - t0) / 1000;
  let distinct = 0, complete = 0, torn = 0, lastSeq = -1, lastComplete = -1;
  for (const s of seen) {
    if (s.seq !== lastSeq) distinct++;
    lastSeq = s.seq;
    if (s.top >= 0 && s.bottom >= 0) {
      if (s.top !== s.bottom) torn++;
      else if (s.top !== lastComplete) (complete++, (lastComplete = s.top));
    }
  }
  const r = {
    name,
    device: {
      changedPerSec: delta("changed") / devSecs,
      halfScreenPerSec: delta("fullChanges") / devSecs,
      fbtickCompletePerSec: delta("complete") / devSecs,
      fbtickTornPerSec: delta("torn") / devSecs,
      fbtickSkipped: delta("skipped"),
      gpuFlushesPerSec: delta("gpuFlushes") / devSecs,
    },
    worker: { presentsPerSec: presents.length / pSecs, gapP50: pct(0.5), gapP95: pct(0.95), gapMax: gaps[gaps.length - 1] ?? 0,
      runP50: presents.map((p) => p[2]).sort((a, b) => a - b)[presents.length >> 1] ?? 0,
      drawP95: presents.map((p) => p[3]).sort((a, b) => a - b)[Math.floor(presents.length * 0.95)] ?? 0 },
    composited: { capturesPerSec: seen.length / span, distinctPresentsPerSec: distinct / span, fbtickCompletePerSec: complete / span, fbtickTornPerSec: torn / span },
  };
  console.log(JSON.stringify(r, null, 2));
  return r;
}

test("display trace", async ({ page }) => {
  test.setTimeout(1_800_000);
  page.on("console", (m) => m.type() === "warning" && console.log("page:", m.text()));
  await page.goto(`./?relay=&trace=display&${QUERY}`);
  const term = page.locator(".xterm-rows");
  await expect(term).toContainText("root@bootlet:~#", { timeout: 600_000 });
  const display = page.locator("#display");
  await expect(display).toBeVisible();
  const info = (await probe(page)).display;
  console.log("display:", JSON.stringify(info));

  const type = async (cmd: string) => {
    await page.locator(".xterm").click();
    await page.keyboard.type(cmd + "\n");
  };
  // fbtick renders its 16 frames on first use; do that before measuring.
  await type("fbtick 1 1; echo re''ady");
  await expect(term).toContainText("ready", { timeout: 600_000 });

  const results = [];
  results.push(await measure(page, "fbtick-10fps", 10, () => type("fbtick 10 12")));
  await expect(term).toContainText("fbtick: wrote 120", { timeout: 120_000 });
  results.push(await measure(page, "doom-forward", 15, async () => {
    await type("DOOM_FPS=1 doom");
    await expect(term).toContainText("I_InitGraphics", { timeout: 120_000 });
    await page.waitForTimeout(3000);
    // Start a game: menu, New Game, episode, skill; then walk forward.
    await display.click();
    for (const key of ["Escape", "Enter", "Enter", "Enter"]) {
      await page.keyboard.press(key);
      await page.waitForTimeout(1500);
    }
    await page.waitForTimeout(2000); // past the level-start wipe
    await page.keyboard.down("ArrowUp");
  }));
  await page.keyboard.up("ArrowUp");
  const doomOut = (await term.innerText()).split("\n").filter((l) => l.includes("DOOM FPS"));
  console.log(doomOut.join("\n"));

  mkdirSync(OUT, { recursive: true });
  writeFileSync(join(OUT, `display-trace-${LABEL}.json`), JSON.stringify({ query: QUERY, display: info, results, doom: doomOut }, null, 2));
});
