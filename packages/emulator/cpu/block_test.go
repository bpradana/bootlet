package cpu_test

import (
	"encoding/binary"
	"testing"

	"bootlet/emulator/cpu"
)

const (
	addiA0_1 = 0x00150513 // addi a0, a0, 1
	addiA0_2 = 0x00250513 // addi a0, a0, 2
	swT1T0   = 0x0062a023 // sw t1, 0(t0)
	jalBack4 = 0xffdff06f // jal x0, -4
	jalBack8 = 0xff9ff06f // jal x0, -8
)

func program(words ...uint32) []byte {
	ram := make([]byte, 1<<16)
	for i, w := range words {
		binary.LittleEndian.PutUint32(ram[4*i:], w)
	}
	return ram
}

// A store that rewrites an instruction of the running block takes effect on
// the next execution, without fence.i, exactly as in the interpreter.
func TestBlocksSelfModifyingCode(t *testing.T) {
	for _, blocks := range []bool{false, true} {
		ram := program(addiA0_1, swT1T0, jalBack8)
		c := cpu.New(ram, base, nobus{})
		c.UseBlocks(blocks)
		c.X[5], c.X[6] = base, addiA0_2 // t0: the addi, t1: its replacement
		c.Run(3)                        // addi(+1), sw (patches the addi), jal
		c.Run(3)                        // the patched addi (+2)
		if c.X[10] != 3 {
			t.Errorf("blocks=%v: a0 = %d, want 3", blocks, c.X[10])
		}
	}
}

// Memory written behind the CPU's back (device DMA) is re-decoded after
// InvalidateRange.
func TestBlocksInvalidateRange(t *testing.T) {
	ram := program(addiA0_1, jalBack4)
	c := cpu.New(ram, base, nobus{})
	c.UseBlocks(true)
	c.Run(4) // two iterations: a0 = 2
	binary.LittleEndian.PutUint32(ram, addiA0_2)
	c.InvalidateRange(base, 4)
	c.Run(4) // two more, now adding 2
	if c.X[10] != 6 {
		t.Fatalf("a0 = %d, want 6", c.X[10])
	}
	if built, hits, _, inval := c.BlockStats(); built < 2 || hits == 0 || inval != 1 {
		t.Errorf("stats: built %d hits %d invalidations %d", built, hits, inval)
	}
}

// A block that reaches the end of a page stops there: with paging, the next
// virtual page maps to an unrelated physical page. Here the next physical page
// is changed without invalidation; a block that ran on into it would execute
// a stale decoded copy.
func TestBlocksStopAtPageEnd(t *testing.T) {
	ram := make([]byte, 1<<16)
	binary.LittleEndian.PutUint16(ram[0xffe:], 0x0505) // c.addi a0, 1 (last slot of page 0)
	binary.LittleEndian.PutUint32(ram[0x1000:], addiA0_2)
	c := cpu.New(ram, base, nobus{})
	c.UseBlocks(true)
	c.Reset(base + 0xffe)
	c.Run(1) // interpreter (no cached translation yet): a0 = 1
	c.PC = base + 0xffe
	c.Run(1) // decodes the block at 0xffe: a0 = 2
	if built, _, _, _ := c.BlockStats(); built != 1 {
		t.Fatalf("built %d blocks, want 1", built)
	}
	binary.LittleEndian.PutUint32(ram[0x1000:], 0x06450513) // addi a0, a0, 100
	c.PC = base + 0xffe
	c.Run(2) // the block (+1), then the new instruction on the next page (+100)
	if c.X[10] != 103 {
		t.Fatalf("a0 = %d, want 103: the block ran past the end of its page", c.X[10])
	}
}
