// Go's lib/wasm/wasm_exec_node.js, plus globalThis.bootletWasmMemory (see
// go_js_wasm_exec_jit).
"use strict";

if (process.argv.length < 3) {
  console.error("usage: go_js_wasm_exec_jit [wasm binary] [arguments]");
  process.exit(1);
}

globalThis.require = require;
globalThis.fs = require("fs");
globalThis.path = require("path");
globalThis.TextEncoder = require("util").TextEncoder;
globalThis.TextDecoder = require("util").TextDecoder;
globalThis.performance ??= require("performance");
globalThis.crypto ??= require("crypto");

require(require("path").join(process.env.BOOTLET_GOROOT, "lib/wasm/wasm_exec.js"));

const go = new Go();
// The JIT's direct entry (cpu.SetJITRun): Go sets bootletJit.run.
globalThis.bootletJit = { run: () => 2 /* no JIT yet: miss */ };
go.importObject.bootlet = { jit_run: () => globalThis.bootletJit.run() };
go.argv = process.argv.slice(2);
// wasm_exec.js limits argv + environment to a few KB: pass what tests use.
const keep = /^(BOOTLET_|GUEST_OUT$|HOME$|PWD$|TMPDIR$|GOCOVERDIR$)/;
go.env = Object.fromEntries(Object.entries(process.env).filter(([k]) => keep.test(k)));
go.env.TMPDIR ??= require("os").tmpdir();
go.exit = process.exit;
WebAssembly.instantiate(fs.readFileSync(process.argv[2]), go.importObject)
  .then((result) => {
    globalThis.bootletWasmMemory = result.instance.exports.mem;
    process.on("exit", (code) => {
      if (code === 0 && !go.exited) {
        go._pendingEvent = { id: 0 };
        go._resume();
      }
    });
    return go.run(result.instance);
  })
  .catch((err) => {
    console.error(err);
    process.exit(1);
  });
