//go:build js && wasm

package machine_test

import "bootlet/emulator/cpu"

//go:wasmimport bootlet jit_run
func jitRun() int32

func init() { cpu.SetJITRun(jitRun) }
