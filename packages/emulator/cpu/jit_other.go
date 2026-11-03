//go:build !(js && wasm)

package cpu

// The JIT needs a WebAssembly host to compile its output; native builds run
// the block cache.
func newJITBackend() jitBackend { return nil }
