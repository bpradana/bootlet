//go:build js && wasm

package cpu_test

import "bootlet/emulator/cpu"

//go:wasmimport bootlet jit_run
func jitRun() int32

func init() { cpu.SetJITRun(jitRun) }
