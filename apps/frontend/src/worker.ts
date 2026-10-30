// Runs the Go/WASM emulator off the main thread. The emulator executes in
// short slices; between slices the worker posts console output and handles
// incoming messages, and while the guest idles (wfi) it sleeps until the next
// timer event or keyboard input.
import "./vendor/wasm_exec.js";
import { assets, type FromWorker, type ToWorker } from "@bootlet/shared";
import { openStore, type BlockStore } from "./storage";

const SLICE_MS = 12;
const DISK_MIB = 64;
const SAVE_INTERVAL_MS = 2000;

let api: BootletAPI | undefined;
let baseURL = "";
let booting: Promise<void> | undefined;
let running = false;
let idleTimer: ReturnType<typeof setTimeout> | undefined;
let store: BlockStore | undefined;
let saving = false;
let instructions = 0;
let statsSince = performance.now();

const post = (msg: FromWorker, transfer: Transferable[] = []) => postMessage(msg, transfer);

// MessageChannel wakeups avoid setTimeout's 4ms clamping between busy slices.
const channel = new MessageChannel();
channel.port1.onmessage = () => slice();
const scheduleNow = () => channel.port2.postMessage(null);

async function fetchAsset(url: string, label: string): Promise<Uint8Array> {
  const res = await fetch(new URL(url, baseURL));
  if (!res.ok || !res.body) throw new Error(`${url}: HTTP ${res.status}`);
  const total = Number(res.headers.get("content-length")) || 0;
  const chunks: Uint8Array[] = [];
  let loaded = 0;
  const reader = res.body.getReader();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    loaded += value.length;
    post({ type: "progress", label, loaded, total });
  }
  let data = new Uint8Array(loaded);
  let off = 0;
  for (const c of chunks) {
    data.set(c, off);
    off += c.length;
  }
  // Decompress unless the server already did (Content-Encoding: gzip).
  if (data[0] === 0x1f && data[1] === 0x8b) {
    const stream = new Blob([data]).stream().pipeThrough(new DecompressionStream("gzip"));
    data = new Uint8Array(await new Response(stream).arrayBuffer());
  }
  return data;
}

async function boot() {
  post({ type: "state", state: "loading" });
  const go = new Go();
  const disk = openStore().then(
    async (s) => ({ store: s, blocks: await s.load() }),
    (e: unknown) => ({ error: String(e instanceof Error ? e.message : e) }),
  );
  const [wasm, kernel, initrd] = await Promise.all([
    fetchAsset(assets.wasm, "emulator").then((b) => WebAssembly.instantiate(b.buffer as ArrayBuffer, go.importObject)),
    fetchAsset(assets.kernel, "kernel"),
    fetchAsset(assets.initrd, "initramfs"),
  ]);
  void go.run(wasm.instance); // returns once Go's main blocks; the API is then registered
  api = (globalThis as unknown as { bootlet: BootletAPI }).bootlet;
  api.create(0, "");
  check(api.loadKernel(kernel));
  api.loadInitrd(initrd);
  const d = await disk;
  if ("store" in d) {
    store = d.store;
    api.attachDisk(DISK_MIB, d.blocks.idx, d.blocks.data);
    post({ type: "storage", backend: store.kind });
    setInterval(save, SAVE_INTERVAL_MS);
  } else {
    post({ type: "storage", backend: "none", detail: d.error });
  }
  check(api.reset());
  post({ type: "state", state: "booting" });
}

// save persists disk blocks written since the last call.
async function save() {
  if (!api || !store || saving) return;
  const dirty = api.takeDirty();
  if (!dirty) return;
  saving = true;
  try {
    await store.save(dirty);
  } catch (e) {
    post({ type: "storage", backend: "none", detail: `saving failed: ${e}` });
  } finally {
    saving = false;
  }
}

function check(err: string | null) {
  if (err) throw new Error(err);
}

function slice() {
  if (!running || !api) return;
  idleTimer = undefined;
  const r = api.run(SLICE_MS);
  if (r.output.length > 0) post({ type: "output", data: r.output }, [r.output.buffer]);
  instructions += r.instructions;
  const now = performance.now();
  if (now - statsSince > 1000) {
    post({ type: "stats", mips: instructions / (now - statsSince) / 1000 });
    instructions = 0;
    statsSince = now;
  }
  if (r.halted) {
    running = false;
    void save();
    post({ type: "state", state: "halted" });
    return;
  }
  if (!r.idle) {
    scheduleNow();
  } else if (r.idleMs >= 0) {
    idleTimer = setTimeout(slice, Math.max(1, r.idleMs));
  } // else: only input can wake the guest
}

function wake() {
  if (!running) return;
  if (idleTimer !== undefined) clearTimeout(idleTimer);
  idleTimer = undefined;
  scheduleNow();
}

function pause() {
  running = false;
  if (idleTimer !== undefined) clearTimeout(idleTimer);
  idleTimer = undefined;
}

async function start() {
  try {
    booting ??= boot();
    await booting;
  } catch (e) {
    booting = undefined;
    post({ type: "state", state: "error", detail: String(e) });
    return;
  }
  if (running) return;
  running = true;
  post({ type: "state", state: "running" });
  wake();
}

self.onmessage = async (ev: MessageEvent<ToWorker>) => {
  const msg = ev.data;
  switch (msg.type) {
    case "start":
      baseURL ||= msg.base;
      await start();
      break;
    case "stop":
      pause();
      void save();
      if (api) post({ type: "state", state: "stopped" });
      break;
    case "restart":
      if (!api) return start();
      pause();
      check(api.reset());
      post({ type: "state", state: "booting" });
      await start();
      break;
    case "input":
      api?.input(msg.data);
      wake();
      break;
  }
};
