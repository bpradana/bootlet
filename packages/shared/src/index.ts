// Message protocol between the UI thread and the emulator Web Worker.

export type VMState = "loading" | "booting" | "running" | "stopped" | "halted" | "error";

/** Guest network status: "off" when no relay is configured. */
export type NetState = "off" | "connecting" | "online" | "offline";

/** Messages sent from the UI to the worker. */
export type ToWorker =
  // Boot on first use, resume when stopped. base: page URL for assets;
  // relay: Wisp relay WebSocket URL ("" for no network); memMiB: guest RAM;
  // quiet: hide the kernel's boot log; display: add the display, driven by
  // displayDevice ("simplefb", the default, or "virtio-gpu");
  // splash: show the power-on screen on it until Linux draws;
  // deterministic: guest time from the instruction count (benchmarks).
  | {
      type: "start";
      base: string;
      relay: string;
      memMiB: number;
      quiet: boolean;
      display: boolean;
      displayDevice: string;
      splash: boolean;
      deterministic: boolean;
      engine: string; // "blocks" (default) or "interp"
    }
  | { type: "stats" } // post stats now (benchmarks)
  | { type: "stop" } // pause execution
  | { type: "restart" } // reboot from the original images
  | { type: "resetDisk" } // wipe the persistent disk and reboot
  | { type: "relay"; url: string } // connect to another relay ("" disconnects)
  | { type: "input"; data: Uint8Array }
  | { type: "agent"; data: Uint8Array } // bytes for the guest's file agent
  // The display canvas, transferred so the worker draws into it directly;
  // renderer: "webgpu" or "2d" ("" picks WebGPU when available).
  | { type: "canvas"; canvas: OffscreenCanvas; renderer: string }
  // The canvas's displayed size in device pixels (WebGPU draws at it).
  | { type: "canvasSize"; width: number; height: number }
  // The page can't hand over its canvas (no OffscreenCanvas): frames come
  // back as "frame" messages instead.
  | { type: "frames" }
  // Redraw the whole screen on the next frame.
  | { type: "redraw" }
  // A key on the display's keyboard: code is a Linux input KEY_* code.
  | { type: "key"; code: number; down: boolean };

/** Messages sent from the worker to the UI. */
export type FromWorker =
  | { type: "state"; state: VMState; detail?: string }
  | { type: "progress"; label: string; loaded: number; total: number }
  | { type: "output"; data: Uint8Array }
  // mips: guest instructions per wall-clock second (idle time included);
  // activeMips: per second spent executing; fps: display frames presented.
  // totals: counted since the machine was created, for benchmarks.
  | {
      type: "stats";
      mips: number;
      activeMips: number;
      fps: number;
      memMiB: number;
      totalInstructions: number;
      totalBusyMs: number;
      totalFrames: number;
      engine: EngineStats;
      display: DisplayStats;
    }
  // Changed display regions as [x, y, w, h, offset] quintuples, with their
  // RGBA pixels packed row by row from offset in data.
  | { type: "frame"; rects: Int32Array; data: Uint8Array }
  // WebGPU stopped working: the page should hand over a new canvas for 2D.
  | { type: "displayFallback" }
  | { type: "storage"; backend: "opfs" | "indexeddb" | "none"; detail?: string }
  | { type: "net"; state: NetState }
  | { type: "agent"; data: Uint8Array }; // bytes from the guest's file agent

/** How guest code runs: the interpreter, the decoded-block cache, or the
 * WebAssembly JIT (with its counters since the machine was created). */
export type EngineStats = { engine: string; compiled: number; codeKiB: number; compileMs: number; entries: number };

/** The display's device ("simplefb", "virtio-gpu" or "none") and how the
 * page draws it ("webgpu", "2d" or "none"). */
export type DisplayStats = { device: string; renderer: string };

/** The worker's once-a-second emulator statistics. */
export type EmulatorStats = Extract<FromWorker, { type: "stats" }>;

/** Paths of the static assets, relative to the page. */
export const assets = {
  wasm: "bootlet.wasm",
  kernel: "guest/Image.gz",
  initrd: "guest/initrd.cpio.gz",
} as const;

/** Size of the guest display. */
export const display = { width: 640, height: 480 } as const;

/** Guest console prompt, used to detect that boot completed. */
export const shellPrompt = "root@bootlet:";

export * from "./agent";
