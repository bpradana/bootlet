// Go's wasm_exec.js (copied from GOROOT by `mise run wasm:build`) defines Go.
declare class Go {
  importObject: WebAssembly.Imports;
  run(instance: WebAssembly.Instance): Promise<void>;
}

declare module "*/wasm_exec.js";

/** The API registered by packages/emulator/cmd/wasm. */
interface BootletAPI {
  create(ramMiB: number, cmdline: string, quiet: boolean): void;
  loadKernel(image: Uint8Array): string | null;
  loadInitrd(cpio: Uint8Array): void;
  reset(): string | null;
  run(budgetMs: number): {
    output: Uint8Array;
    halted: boolean;
    idle: boolean;
    idleMs: number;
    instructions: number;
  };
  input(data: Uint8Array): void;
  attachDisk(sizeMiB: number, idx: Uint32Array, data: Uint8Array): void;
  takeDirty(): { idx: Uint32Array; data: Uint8Array } | null;
  resetDisk(sizeMiB: number): void;
  attachNet(): void;
  netInput(packet: Uint8Array): void;
  netOutput(): Uint8Array<ArrayBuffer>[];
  netConnecting(): void;
  netDisconnected(): void;
}

interface ImportMetaEnv {
  /** Default Wisp relay URL baked in at build time (optional). */
  readonly VITE_RELAY_URL?: string;
}
