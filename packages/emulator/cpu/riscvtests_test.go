package cpu_test

import (
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

func TestRISCVTests(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(testsDir, "*", "*"))
	if len(files) == 0 {
		t.Skip("riscv-tests not built; run `mise run emu:riscv-tests`")
	}
	for _, f := range files {
		name := filepath.Base(filepath.Dir(f)) + "-p-" + filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runTest(t, f)
		})
	}
}

func runTest(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const base = 0x80000000
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
