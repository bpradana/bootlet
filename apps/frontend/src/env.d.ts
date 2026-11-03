// Go's wasm_exec.js (copied from GOROOT by `mise run wasm:build`) defines Go.
declare class Go {
  importObject: WebAssembly.Imports;
  run(instance: WebAssembly.Instance): Promise<void>;
}

declare module "*/wasm_exec.js";

/** See EngineStats in @bootlet/shared. */
type EngineStats = { engine: string; compiled: number; codeKiB: number; compileMs: number; entries: number };

/** The API registered by packages/emulator/cmd/wasm. */
interface BootletAPI {
  create(ramMiB: number, cmdline: string, quiet: boolean, opts?: { display: boolean; displayDevice: string; splash: boolean; deterministic: boolean; engine: string }): void;
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
  /** OPFS: blocks are read on demand. IndexedDB: every stored block, restored into memory. */
  attachDisk(
    sizeMiB: number,
    source: { empty: boolean; readBlocks(idx: Uint32Array, out: Uint8Array): void } | { idx: Uint32Array; data: Uint8Array },
  ): void;
  /** Confirms the oldest unconfirmed takeDirty() batch was persisted. */
  diskSaved(): void;
  /** Emulator memory use in MiB. */
  memory(): number;
  takeDirty(): { idx: Uint32Array; data: Uint8Array } | null;
  resetDisk(sizeMiB: number): void;
  attachNet(): void;
  netInput(packet: Uint8Array): void;
  netOutput(): Uint8Array<ArrayBuffer>[];
  netConnecting(): void;
  netDisconnected(): void;
  /** The file agent's console port: bytes in both directions. */
  attachAgent(): void;
  agentInput(data: Uint8Array): void;
  agentOutput(): Uint8Array<ArrayBuffer> | null;
  /** Copies changed display regions into out (RGBA, a whole screen's worth);
   * returns them as [x, y, w, h, offset] quintuples, or null if none changed. */
  displayFlush(out: Uint8Array): Int32Array | null;
  displayInvalidate(): void;
  /** A key press or release on the virtio keyboard (Linux KEY_* code). */
  keyEvent(code: number, down: boolean): void;
  /** How guest code runs: "interp", "blocks" or "jit", with JIT counters. */
  engineStats(): EngineStats;
}

interface ImportMetaEnv {
  /** Default Wisp relay URL baked in at build time (optional). */
  readonly VITE_RELAY_URL?: string;
}
