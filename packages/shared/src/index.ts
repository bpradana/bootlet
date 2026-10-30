// Message protocol between the UI thread and the emulator Web Worker.

export type VMState = "loading" | "booting" | "running" | "stopped" | "halted" | "error";

/** Messages sent from the UI to the worker. */
export type ToWorker =
  | { type: "start"; base: string } // boot on first use, resume when stopped; base = page URL for assets
  | { type: "stop" } // pause execution
  | { type: "restart" } // reboot from the original images
  | { type: "input"; data: Uint8Array };

/** Messages sent from the worker to the UI. */
export type FromWorker =
  | { type: "state"; state: VMState; detail?: string }
  | { type: "progress"; label: string; loaded: number; total: number }
  | { type: "output"; data: Uint8Array }
  | { type: "stats"; mips: number }
  | { type: "storage"; backend: "opfs" | "indexeddb" | "none"; detail?: string };

/** Paths of the static assets, relative to the page. */
export const assets = {
  wasm: "bootlet.wasm",
  kernel: "guest/Image.gz",
  initrd: "guest/initrd.cpio.gz",
} as const;

/** Guest console prompt, used to detect that boot completed. */
export const shellPrompt = "root@bootlet:";
