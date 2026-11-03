package cpu_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"bootlet/emulator/boot"
	"bootlet/emulator/cpu"
)

// riscv-tests ELFs, built by `mise run emu:riscv-tests`.
const testsDir = "../../../third_party/riscv-tests-build"

type nobus struct{}

func (nobus) Read(uint64, int) (uint64, bool) { return 0, false }
func (nobus) Write(uint64, int, uint64) bool  { return false }

// TestRISCVTests runs the official tests on both execution engines.
func TestRISCVTests(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(testsDir, "*", "*"))
	if len(files) == 0 {
		t.Skip("riscv-tests not built; run `mise run emu:riscv-tests`")
	}
	for _, blocks := range []bool{false, true} {
		engine := map[bool]string{false: "interp", true: "blocks"}[blocks]
		for _, f := range files {
			name := engine + "/" + filepath.Base(filepath.Dir(f)) + "-p-" + filepath.Base(f)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				runTest(t, f, blocks)
			})
		}
	}
}

// TestEngineLockstep runs every test on both engines side by side and
// compares the complete architectural state (registers, PC, privilege,
// instruction count, memory) after every slice of instructions.
func TestEngineLockstep(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(testsDir, "*", "*"))
	if len(files) == 0 {
		t.Skip("riscv-tests not built; run `mise run emu:riscv-tests`")
	}
	for _, f := range files {
		name := filepath.Base(filepath.Dir(f)) + "-p-" + filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, ramA, tohost := load(t, f)
			b, ramB, _ := load(t, f)
			b.UseBlocks(true)
			for step := 0; step < 20000; step++ {
				n := 1 + step%53 // vary slice boundaries
				a.Run(n)
				b.Run(n)
				if a.X != b.X || a.F != b.F || a.PC != b.PC || a.Priv != b.Priv || a.Cycles != b.Cycles {
					t.Fatalf("after %d instructions: interp pc=%#x priv=%d x=%x\nblocks pc=%#x priv=%d x=%x",
						a.Cycles, a.PC, a.Priv, a.X, b.PC, b.Priv, b.X)
				}
				if !bytes.Equal(ramA, ramB) {
					t.Fatalf("memory differs after %d instructions (pc=%#x)", a.Cycles, a.PC)
				}
				if binary.LittleEndian.Uint64(ramA[tohost-base:]) != 0 {
					return
				}
			}
		})
	}
}

const base = 0x80000000

func load(t *testing.T, path string) (*cpu.CPU, []byte, uint64) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ram := make([]byte, 4<<20)
	e, err := boot.LoadELF(ram, base, data)
	if err != nil {
		t.Fatal(err)
	}
	tohost, ok := e.Symbols["tohost"]
	if !ok {
		t.Fatal("no tohost symbol")
	}
	c := cpu.New(ram, base, nobus{})
	c.Reset(e.Entry)
	return c, ram, tohost
}

func runTest(t *testing.T, path string, blocks bool) {
	c, ram, tohost := load(t, path)
	c.UseBlocks(blocks)
	for i := 0; i < 1000; i++ {
		c.Run(10000)
		if v := binary.LittleEndian.Uint64(ram[tohost-base:]); v != 0 {
			if v != 1 {
				t.Fatalf("failed test case %d (pc=%#x)", v>>1, c.PC)
			}
			return
		}
	}
	t.Fatalf("timed out (pc=%#x)", c.PC)
}
