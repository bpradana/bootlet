# Follow-up: translating guest code to WebAssembly

Status: proposal, not implemented. The decoded-block cache (`cpu/block.go`) is the
current fast path. This note scopes the next step so it can be planned on its own.

## Why

After the block cache, the browser runs guest code at about 35–55 MIPS. What remains is
interpretive: a `switch` per decoded operation, guest registers in memory (`c.X`), and a
Go function call for every load and store. Natively the same Go code is 3–5× faster, so
most of the gap is Go's WebAssembly code generation, not the emulator's design.
Translating hot blocks to WebAssembly would turn each guest instruction into a few Wasm
instructions that the browser's optimizing compiler handles. ClairEmu (TinyEMU-based)
does this: its loader passes generated module bytes to a JavaScript helper that compiles
and instantiates them against the shared memory and function table.

## Constraints specific to bootlet

- **Go cannot emit and run code itself.** Under `GOOS=js` the Go runtime owns its
  memory and table, and there is no API to add functions to its table. Generated modules
  must be compiled by JavaScript (`new WebAssembly.Module(bytes)`) and called through a
  `js.Func`. Each Go→JS→Wasm transition costs microseconds, so a call per block would
  cancel the gain. The translated code has to run many blocks per call: chained blocks, or
  a loop region translated as one function.
- **Memory sharing.** Translated code needs direct access to guest RAM and the CPU
  state. Options:
  - Import Go's `WebAssembly.Memory` into the generated modules and pass addresses of
    `c.RAM` and `c.X`. This only works as long as the Go GC never moves them; they are
    large, long-lived allocations, but nothing guarantees it.
  - Move guest RAM into a separate `WebAssembly.Memory` that Go reaches through
    `syscall/js`. That makes every interpreter access slower.
  - Move the CPU core (or the whole emulator) to a toolchain with Wasm control (TinyGo,
    or C/Rust via Emscripten) and keep Go for devices. That is a much larger change.
- **Exactness.** The same rules as the block cache: precise traps (PC and `instret` at
  the faulting instruction), interrupts at the instructions where the interpreter takes
  them, budget limits, invalidation on writes to translated pages, and translation keyed
  by physical address. Memory accesses need an inline TLB lookup with a fallback call
  for misses, MMIO, and faults.

## Proposed scope

1. **Prototype outside the hot path.** A translator for the operations the block cache
   already decodes (integer ALU, loads/stores, branches), with an exit to Go for anything
   else, emitting a Wasm function per block. Measure compile time per block and the
   Go↔JS call overhead in Chromium, Firefox, and Safari.
2. **Decide memory sharing** from those numbers (options above), including the effect of
   SharedArrayBuffer and cross-origin isolation on deployment (GitHub Pages cannot set
   the COOP/COEP headers).
3. **Region translation and chaining**, so one call runs until an exit condition.
4. **Correctness harness:** reuse `TestEngineLockstep` and `TestDeterministic`. The Wasm
   path runs only in the browser, so add a headless-Chromium variant of both.
5. **Tiering:** interpreter → block cache → Wasm for blocks above a hit threshold, with
   statistics (requests, cache hits, translated blocks, compile time, live code bytes).

Out of scope: the vector extension (bootlet implements RV64GC) and GPU acceleration. The
display is a linear framebuffer for software rendering.
