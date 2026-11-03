package cpu_test

import (
	"encoding/binary"
	"testing"

	"bootlet/emulator/cpu"
)

func jitCPU(t *testing.T, ram []byte) *cpu.CPU {
	*cpu.JITThreshold = 1
	c := cpu.New(ram, base, nobus{})
	if !c.UseJIT() {
		t.Skip("no WebAssembly host (run with GOOS=js GOARCH=wasm and scripts/go_js_wasm_exec_jit)")
	}
	return c
}

// A loop gets compiled and then runs inside compiled code.
func TestJITRunsCompiledCode(t *testing.T) {
	ram := program(addiA0_1, jalBack4)
	c := jitCPU(t, ram)
	for i := 0; i < 50; i++ {
		c.Run(1000)
	}
	if c.X[10] != 25000 {
		t.Fatalf("a0 = %d, want 25000", c.X[10])
	}
	s := c.JITStats()
	if s.Compiled == 0 || s.Entries == 0 {
		t.Fatalf("nothing compiled or entered: %+v", s)
	}
	t.Logf("%+v", s)
}

// Self-modifying code without fence.i, and DMA-style writes, through the JIT.
func TestJITSelfModifyingCode(t *testing.T) {
	ram := program(addiA0_1, swT1T0, jalBack8)
	c := jitCPU(t, ram)
	c.X[5], c.X[6] = base, addiA0_1
	for i := 0; i < 20; i++ {
		c.Run(30) // gets the loop compiled; the store rewrites the addi with itself
	}
	before := c.X[10]
	c.X[6] = addiA0_2 // now the store patches in a different instruction
	c.Run(3)          // addi(+1), sw (patch), jal
	c.Run(3)          // the patched addi (+2)
	if got := c.X[10] - before; got != 3 {
		t.Fatalf("a0 grew by %d, want 3", got)
	}
}

func TestJITInvalidateRange(t *testing.T) {
	ram := program(addiA0_1, jalBack4)
	c := jitCPU(t, ram)
	for i := 0; i < 20; i++ {
		c.Run(100)
	}
	if c.JITStats().Compiled == 0 {
		t.Fatal("not compiled")
	}
	a := c.X[10]
	binary.LittleEndian.PutUint32(ram, addiA0_2)
	c.InvalidateRange(base, 4)
	c.Run(100) // 50 iterations of the new code
	if got := c.X[10] - a; got != 100 {
		t.Fatalf("a0 grew by %d, want 100", got)
	}
}
