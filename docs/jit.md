# The WebAssembly JIT

In the browser, bootlet translates hot guest code to WebAssembly. That is its third
execution tier, above the plain interpreter (`cpu/exec.go`) and the decoded-block
cache (`cpu/block.go`). The JIT is `cpu/jit.go`, with a small Wasm encoder in
`cpu/wasm.go`. It is the default in the page; `?engine=blocks` and `?engine=interp`
select the other tiers. Native builds have no WebAssembly host, so `machine.EngineJIT`
runs the block cache there.

## How it works

1. **Hotness.** The block cache counts how often each decoded block runs. After 24 runs a
   block is queued. When 128 blocks are queued, or after 8 more `Run` calls, the queue
   becomes one WebAssembly module of one function per block.
2. **Compiling.** Go encodes the module and hands the bytes to the host
   (`cpu/jit_js.go`, through `syscall/js`), which compiles it with `new
   WebAssembly.Module` and instantiates it. Instantiation writes the functions into a
   shared `WebAssembly.Table` through an element segment.
3. **Shared state.** Generated code imports the emulator's own WebAssembly memory. The
   worker exposes it as `globalThis.bootletWasmMemory`. The code reads and writes guest
   registers, the PC, `instret`, the TLBs, and guest RAM in place, at their addresses in
   that memory. This is safe because Go's garbage collector never moves heap objects.
4. **The dispatcher.** A generated `run()` function loops:
   - it stops if an interrupt would be taken now (the exact rules of `CPU.interrupt`,
     so a pending but masked interrupt doesn't end the run);
   - it translates the PC through the instruction TLB;
   - it finds the compiled block for that physical address in `jitTab`, a 2-way hash
     table;
   - it checks that the block fits the instruction budget;
   - it calls the block through the table.

   It returns to Go only when a block exits or nothing compiled is found.
5. **Entering.** Go calls `run()` through a `//go:wasmimport bootlet jit_run` import.
   The worker supplies it with a one-line trampoline. A `syscall/js` call costs about a
   microsecond; this costs a plain call.

## Exactness

Compiled code must behave exactly like the interpreter: the same results, the same
`instret`, the same trap state, and every interrupt taken at the same instruction.

- **What is inline.** A block translates its integer ALU, M-extension (except the high
  multiplies, which need a 128-bit product), load/store, `fence`, and control-transfer
  instructions.
- **Exits.** At any other instruction, the block stores the PC and the `instret` so far
  and exits. Go then executes exactly that instruction with the interpreter. That covers
  FP, atomics, CSRs, system instructions, `mulh*`, and memory accesses outside the fast
  path:
  - a TLB miss;
  - an MXR access;
  - an access outside RAM (MMIO);
  - an access crossing a page;
  - a store to a page holding decoded code (Go invalidates that code).
- **Same fast path.** The inline memory path makes the same checks as `CPU.load` and
  `CPU.store`: a TLB hit, the R/W/D bits, U/S with SUM, within RAM, not crossing a
  page.
- **`div`/`rem`.** They follow RISC-V's division by zero and overflow rules with
  explicit branches, since WebAssembly traps there.
- **Invalidation.** Compiled code follows the block cache. `jitTab` entries for a page
  are dropped with its blocks. Recycling the block arena keeps compiled code, because
  memory hasn't changed. Reset drops everything.
- **Verification.** Go's WebAssembly build runs under Node with
  `scripts/go_js_wasm_exec_jit`, which also supplies the memory and the import. So the
  existing tests exercise the JIT without a browser, as part of `mise run emu:test` and
  `emu:boot-test`:
  - the riscv-tests on all three engines;
  - `TestEngineLockstep`, which compares registers, PC, privilege, `instret`, and memory
    with the interpreter after every slice of every test. Tests compile blocks after a
    single run, so nearly everything is translated;
  - JIT tests for compiled loops, self-modifying code without `fence.i`, and DMA
    invalidation;
  - `TestDeterministic`, which requires a deterministic Linux boot to reach the prompt
    after exactly the same instruction count with byte-identical memory, on the JIT as
    on the interpreter.
- **In the browser,** the display e2e test checks that the page runs the JIT and
  compiled code.

## What it costs

- **Generated code.** Blocks average about 630 bytes of WebAssembly. A boot compiles
  about 50,000 blocks in about 1,500 modules, in about 1 s of compile time in total. At
  most 16K compiled blocks (the function table) are live. When it is full, everything is
  dropped and recompiled as it runs again. The browser's machine code for live modules is
  extra memory on top of guest RAM; this is estimated, not measured.
- **Tables.** `jitTab` is 64K entries of 16 bytes (1 MiB).
- **Cold code.** Boot runs a lot of code a few times, so it gains least. Tight loops gain
  most.

## Getting there: what mattered

The first working version was slower than the block cache. It entered compiled code for
only about 20 instructions at a time, so the per-call overhead dominated. In order of
impact:

1. **Stale fetch translation.** After compiled code jumped to another code page, Go's
   cached fetch translation still pointed at the old page. So Go interpreted one
   instruction instead of running the block at the miss PC, and that block was never
   built. Refreshing the translation from the instruction TLB (`CPU.fetchCache`) took
   runs from about 20 to 200–440 instructions per entry.
2. **Masked interrupts.** The dispatcher had stopped whenever anything was pending, so Go
   re-entered it for every block. It now applies the interpreter's enable rules.
3. **Untranslated instructions.** A block that stopped before an untranslated instruction
   fell into a miss that could never be compiled. Translating M, the W shifts, and
   `fence`, and ending blocks with an exact "step" exit, fixed that.
4. **Inline SUM.** The kernel's copies to and from user memory (S mode accessing user
   pages with SUM) now stay inline.
5. **Entry cost.** A direct WebAssembly import replaced the `syscall/js` call for
   entering compiled code.
6. **Table collisions.** A 2-way `jitTab` replaced the direct-mapped one, so colliding
   hot blocks stay compiled, and evicted ones are re-queued.

## Not done yet

- **Registers in locals.** Generated code loads and stores guest registers in memory for
  every instruction. Keeping them in Wasm locals within a block, written back at exits,
  would cut most of that traffic.
- **More instructions inline:** atomics (`lr`/`sc`/`amo*` on the RAM fast path) and the
  common `sstatus` CSR updates. These are the largest remaining sources of exits.
- **Page walks in Wasm.** A TLB miss exits to Go. Walking Sv39 in generated code would
  keep miss-heavy workloads compiled.
- **Direct block chaining:** calling the next compiled block directly instead of
  returning to the dispatcher.
- **Smaller code.** Memory accesses dominate code size. Calling a shared access function
  would shrink modules, at some cost per access.
- **Vector instructions and GPU acceleration** are out of scope. bootlet implements
  RV64GC, and the display is a linear framebuffer for software rendering.
