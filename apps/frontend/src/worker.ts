// Runs the Go/WASM emulator off the main thread. The emulator executes in
// short slices; between slices the worker posts console output and handles
// incoming messages, and while the guest idles (wfi) it sleeps until the next
// timer event, keystroke or network packet. The guest always has a network
// device; while a relay is connected, its TCP connections travel as Wisp
// packets over a WebSocket, and the relay can change at any time.
import "./vendor/wasm_exec.js";
import { assets, type FromWorker, type ToWorker } from "@bootlet/shared";
import { openStore, type BlockStore } from "./storage";

const SLICE_MS = 12;
const DISK_MIB = 1024; // sparse: only written blocks are stored
const SAVE_INTERVAL_MS = 2000;

let api: BootletAPI | undefined;
let baseURL = "";
let relayURL = "";
let memMiB = 0;
let quiet = false;
let ws: WebSocket | undefined;
let reconnectDelay = 1000;
let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
let booting: Promise<void> | undefined;
let running = false;
let idleTimer: ReturnType<typeof setTimeout> | undefined;
let store: BlockStore | undefined;
let saving: Promise<void> | undefined;
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
  api.create(memMiB, "", quiet);
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
  api.attachNet();
  setRelay(relayURL);
  check(api.reset());
  post({ type: "state", state: "booting" });
}

// save persists disk blocks written since the last call.
async function save() {
  if (!api || !store || saving) return;
  const dirty = api.takeDirty();
  if (!dirty) return;
  saving = store
    .save(dirty)
    .catch((e) => post({ type: "storage", backend: "none", detail: `saving failed: ${e}` }))
    .finally(() => (saving = undefined));
  await saving;
}

// setRelay switches to a relay ("" disconnects).
function setRelay(url: string) {
  relayURL = url;
  clearTimeout(reconnectTimer);
  reconnectDelay = 1000;
  const old = ws;
  ws = undefined;
  if (old) {
    old.onclose = null;
    old.close();
    api?.netDisconnected();
    wake();
  }
  if (url) connectRelay();
  else post({ type: "net", state: "off" });
}

function connectRelay() {
  post({ type: "net", state: "connecting" });
  let socket: WebSocket;
  try {
    socket = new WebSocket(relayURL);
  } catch (e) {
    post({ type: "net", state: "offline" });
    return;
  }
  api?.netConnecting(); // guest connections now wait for the relay
  socket.binaryType = "arraybuffer";
  ws = socket;
  socket.onopen = () => {
    reconnectDelay = 1000;
    post({ type: "net", state: "online" });
  };
  socket.onmessage = (ev: MessageEvent<ArrayBuffer>) => {
    api?.netInput(new Uint8Array(ev.data));
    flushNet();
    wake();
  };
  socket.onclose = () => {
    if (ws !== socket) return;
    ws = undefined;
    api?.netDisconnected(); // open connections fail; the guest sees resets
    post({ type: "net", state: "offline" });
    wake();
    reconnectTimer = setTimeout(connectRelay, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 30_000);
  };
}

// flushNet sends the emulator's pending Wisp packets to the relay. The Wisp
// client produces nothing until the relay's greeting arrives, so packets are
// only ever pending while the socket is open.
function flushNet() {
  if (!api || !relayURL) return;
  for (const pkt of api.netOutput()) {
    if (ws?.readyState === WebSocket.OPEN) ws.send(pkt);
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
  flushNet();
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
      if (!booting) {
        baseURL = msg.base;
        relayURL = msg.relay;
        memMiB = msg.memMiB;
        quiet = msg.quiet;
      }
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
    case "resetDisk":
      if (!api || !store) return;
      pause();
      await saving; // don't let an in-flight save land after the wipe
      await store.clear();
      api.resetDisk(DISK_MIB);
      check(api.reset());
      post({ type: "state", state: "booting" });
      await start();
      break;
    case "relay":
      if (api) setRelay(msg.url);
      else relayURL = msg.url;
      break;
    case "input":
      api?.input(msg.data);
      wake();
      break;
  }
};
