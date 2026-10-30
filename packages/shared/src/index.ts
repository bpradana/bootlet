// Message protocol between the UI thread and the emulator Web Worker.

export type VMState = "loading" | "booting" | "running" | "stopped" | "halted" | "error";

/** Guest network status: "off" when no relay is configured. */
export type NetState = "off" | "connecting" | "online" | "offline";

/** Messages sent from the UI to the worker. */
export type ToWorker =
  // Boot on first use, resume when stopped. base: page URL for assets;
  // relay: Wisp relay WebSocket URL ("" for no network); memMiB: guest RAM;
  // quiet: hide the kernel's boot log.
  | { type: "start"; base: string; relay: string; memMiB: number; quiet: boolean }
  | { type: "stop" } // pause execution
  | { type: "restart" } // reboot from the original images
  | { type: "resetDisk" } // wipe the persistent disk and reboot
  | { type: "input"; data: Uint8Array };

/** Messages sent from the worker to the UI. */
export type FromWorker =
  | { type: "state"; state: VMState; detail?: string }
  | { type: "progress"; label: string; loaded: number; total: number }
  | { type: "output"; data: Uint8Array }
  | { type: "stats"; mips: number }
  | { type: "storage"; backend: "opfs" | "indexeddb" | "none"; detail?: string }
  | { type: "net"; state: NetState };

/** Paths of the static assets, relative to the page. */
export const assets = {
  wasm: "bootlet.wasm",
  kernel: "guest/Image.gz",
  initrd: "guest/initrd.cpio.gz",
} as const;

/** Guest console prompt, used to detect that boot completed. */
export const shellPrompt = "root@bootlet:";
