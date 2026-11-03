//go:build js && wasm

package cpu

import (
	"errors"
	"fmt"
	"syscall/js"
)

// jsBackend compiles the JIT's modules with the host's WebAssembly API. The
// host must expose the emulator's own memory as globalThis.bootletWasmMemory
// (the worker and the test runner do) so generated code can share it.
type jsBackend struct {
	wasm, mem, table, imports, runFn js.Value
}

func newJITBackend() jitBackend {
	mem := js.Global().Get("bootletWasmMemory")
	wasm := js.Global().Get("WebAssembly")
	if mem.IsUndefined() || wasm.IsUndefined() {
		return nil
	}
	return &jsBackend{wasm: wasm, mem: mem}
}

func (b *jsBackend) init(dispatcher []byte, tableN uint32) (err error) {
	defer catch(&err)
	desc := js.Global().Get("Object").New()
	desc.Set("element", "anyfunc")
	desc.Set("initial", tableN)
	b.table = b.wasm.Get("Table").New(desc)
	env := js.Global().Get("Object").New()
	env.Set("m", b.mem)
	env.Set("t", b.table)
	b.imports = js.Global().Get("Object").New()
	b.imports.Set("env", env)
	inst, err := b.instantiate(dispatcher)
	if err != nil {
		return err
	}
	b.runFn = inst.Get("exports").Get("run")
	// For the direct import (SetJITRun): the host's bootlet.jit_run calls
	// globalThis.bootletJit.run.
	if hook := js.Global().Get("bootletJit"); hook.Type() == js.TypeObject {
		hook.Set("run", b.runFn)
	}
	return nil
}

func (b *jsBackend) instantiate(code []byte) (inst js.Value, err error) {
	defer catch(&err)
	u8 := js.Global().Get("Uint8Array").New(len(code))
	js.CopyBytesToJS(u8, code)
	mod := b.wasm.Get("Module").New(u8)
	return b.wasm.Get("Instance").New(mod, b.imports), nil
}

func (b *jsBackend) load(code []byte) error {
	_, err := b.instantiate(code)
	return err
}

func (b *jsBackend) run() int32 { return int32(b.runFn.Invoke().Int()) }

// catch turns a JavaScript exception (a panic with js.Error) into an error.
func catch(err *error) {
	if r := recover(); r != nil {
		if e, ok := r.(js.Error); ok {
			*err = errors.New(e.Error())
			return
		}
		*err = fmt.Errorf("%v", r)
	}
}
