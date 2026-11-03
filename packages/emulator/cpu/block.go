package cpu

import "encoding/binary"

// Decoded-block cache.
//
// With UseBlocks set, Run executes straight-line runs of instructions that
// were decoded once, instead of fetching and decoding every instruction
// again. The interpreter (exec) stays the reference: anything the block
// engine does not decode itself runs through exec, and the two paths must
// give identical results, down to instruction counts and trap state.
//
// Blocks are keyed by the host RAM offset of their first instruction (a
// physical address), so they survive satp writes, sfence.vma and privilege
// changes: a block is only entered through the normal fetch translation of
// the current PC. A block never crosses a 4 KiB page and ends at the first
// control transfer, system instruction or instruction that changed the PC
// unexpectedly (a trap), so translation and interrupt state are re-checked
// at least every blockMax instructions, as Run does between instructions.
//
// Invalidation: every RAM page holding decoded code is flagged in codePage;
// a store to a flagged page (from the CPU, or DMA and other host writes
// reported with InvalidateRange) drops that page's blocks, so instruction
// fetch is always coherent with memory and fence.i has nothing to do. Reset
// and a full arena drop everything. Memory use is fixed: the table and op
// arena are allocated once.

const (
	blockMax     = 64      // instructions per block
	blockTable   = 1 << 15 // hash table entries (24 B each)
	blockArena   = 1 << 18 // decoded instructions across all blocks (24 B each)
	blockMaskTab = blockTable - 1
)

// Decoded instruction kinds. Anything not listed runs through exec.
const (
	kExec    uint8 = iota // fallback: c.exec(insn)
	kExecEnd              // fallback that ends the block (system, CSR, fence)
	kLUI
	kAUIPC
	kADDI
	kSLTI
	kSLTIU
	kXORI
	kORI
	kANDI
	kSLLI
	kSRLI
	kSRAI
	kADDIW
	kSLLIW
	kSRLIW
	kSRAIW
	kADD
	kSUB
	kSLL
	kSLT
	kSLTU
	kXOR
	kSRL
	kSRA
	kOR
	kAND
	kADDW
	kSUBW
	kLB
	kLH
	kLW
	kLD
	kLBU
	kLHU
	kLWU
	kSB
	kSH
	kSW
	kSD
	// Control transfers: always the last instruction of a block.
	kJAL
	kJALR
	kBEQ
	kBNE
	kBLT
	kBGE
	kBLTU
	kBGEU
)

type dop struct {
	imm          uint64
	insn         uint32 // expanded 32-bit encoding (for exec)
	kind         uint8
	rd, rs1, rs2 uint8
	length       uint8 // 2 or 4
}

type blockEntry struct {
	key   uint64 // RAM offset + 1 (0: empty)
	gen   uint32
	start uint32 // first op in the arena
	n     uint16
}

type blockCache struct {
	table    []blockEntry
	ops      []dop
	top      int
	gen      uint32
	codePage []uint8 // per RAM page: 1 if it holds decoded code

	// Statistics.
	Built, Hits, Flushes, PageInvalidations uint64
}

// UseBlocks selects the decoded-block engine for Run (the interpreter is
// used otherwise).
func (c *CPU) UseBlocks(on bool) {
	if on && c.blocks == nil {
		c.blocks = &blockCache{
			table:    make([]blockEntry, blockTable),
			ops:      make([]dop, blockArena),
			codePage: make([]uint8, (len(c.RAM)+0xfff)>>12),
			gen:      1,
		}
	}
	c.blockMode = on
	c.FlushBlocks()
}

// BlockStats returns the block cache counters (zero when it is off).
func (c *CPU) BlockStats() (built, hits, flushes, pageInvalidations uint64) {
	if b := c.blocks; b != nil {
		return b.Built, b.Hits, b.Flushes, b.PageInvalidations
	}
	return
}

// FlushBlocks discards every decoded block (fence.i, reset).
func (c *CPU) FlushBlocks() {
	b := c.blocks
	if b == nil {
		return
	}
	b.gen++
	if b.gen == 0 { // wrapped: clear stale entries for real
		clear(b.table)
		b.gen = 1
	}
	b.top = 0
	clear(b.codePage)
	b.Flushes++
}

// InvalidateRange drops decoded blocks overlapping guest physical memory
// [pa, pa+n), for writes the CPU does not see (device DMA).
func (c *CPU) InvalidateRange(pa, n uint64) {
	b := c.blocks
	if b == nil || n == 0 {
		return
	}
	off := pa - c.RAMBase
	if off >= uint64(len(c.RAM)) {
		return
	}
	end := min(off+n, uint64(len(c.RAM)))
	for p := off >> 12; p <= (end-1)>>12; p++ {
		if b.codePage[p] != 0 {
			c.invalidatePage(p)
		}
	}
}

// invalidatePage drops the blocks on RAM page p (a store hit decoded code).
func (c *CPU) invalidatePage(p uint64) {
	b := c.blocks
	b.codePage[p] = 0
	b.PageInvalidations++
	for i := range b.table {
		e := &b.table[i]
		if e.key != 0 && (e.key-1)>>12 == p {
			e.key = 0
		}
	}
}

// codeStore is called by stores to RAM offset off; it invalidates decoded
// code there. Kept tiny so the check inlines into the store fast path.
func (c *CPU) codeStore(off uint64) {
	if b := c.blocks; b != nil && b.codePage[off>>12] != 0 {
		c.invalidatePage(off >> 12)
	}
}

// lookup returns the block starting at RAM offset off, decoding it if needed.
func (c *CPU) lookup(off uint64) *blockEntry {
	b := c.blocks
	e := &b.table[(off>>1)&blockMaskTab]
	if e.key == off+1 && e.gen == b.gen {
		b.Hits++
		return e
	}
	if b.top+blockMax > len(b.ops) {
		c.FlushBlocks()
	}
	start := b.top
	first := off
	n := 0
	for n < blockMax {
		// Never leave the first instruction's page: the next physical page
		// need not be the next virtual one.
		if off>>12 != first>>12 {
			break
		}
		po := off & 0xfff
		lo := uint32(binary.LittleEndian.Uint16(c.RAM[off:]))
		var insn uint32
		var length uint8
		if lo&3 == 3 {
			if po > 0xffc {
				break // crosses the page: leave it to the interpreter
			}
			insn, length = binary.LittleEndian.Uint32(c.RAM[off:]), 4
		} else {
			insn, length = rvc[uint16(lo)], 2
			if insn == 0 {
				break // illegal: the interpreter raises it
			}
		}
		op := decode(insn, length)
		b.ops[start+n] = op
		n++
		off += uint64(length)
		if op.kind >= kJAL || op.kind == kExecEnd {
			break
		}
	}
	if n == 0 {
		return nil
	}
	b.top += n
	b.codePage[first>>12] = 1
	*e = blockEntry{key: first + 1, gen: b.gen, start: uint32(start), n: uint16(n)}
	b.Built++
	return e
}

func decode(insn uint32, length uint8) dop {
	op := dop{insn: insn, length: length, kind: kExec,
		rd: uint8(insn >> 7 & 0x1f), rs1: uint8(insn >> 15 & 0x1f), rs2: uint8(insn >> 20 & 0x1f)}
	f3 := insn >> 12 & 7
	f7 := insn >> 25
	switch insn & 0x7f {
	case 0x37:
		op.kind, op.imm = kLUI, immU(insn)
	case 0x17:
		op.kind, op.imm = kAUIPC, immU(insn)
	case 0x6f:
		op.kind, op.imm = kJAL, immJ(insn)
	case 0x67:
		if f3 == 0 {
			op.kind, op.imm = kJALR, immI(insn)
		}
	case 0x63:
		op.imm = immB(insn)
		switch f3 {
		case 0:
			op.kind = kBEQ
		case 1:
			op.kind = kBNE
		case 4:
			op.kind = kBLT
		case 5:
			op.kind = kBGE
		case 6:
			op.kind = kBLTU
		case 7:
			op.kind = kBGEU
		}
	case 0x03:
		op.imm = immI(insn)
		if f3 != 7 {
			op.kind = [...]uint8{kLB, kLH, kLW, kLD, kLBU, kLHU, kLWU}[f3]
		}
	case 0x23:
		op.imm = immS(insn)
		if f3 <= 3 {
			op.kind = [...]uint8{kSB, kSH, kSW, kSD}[f3]
		}
	case 0x13:
		op.imm = immI(insn)
		switch f3 {
		case 0:
			op.kind = kADDI
		case 1:
			if insn>>26 == 0 {
				op.kind, op.imm = kSLLI, op.imm&63
			}
		case 2:
			op.kind = kSLTI
		case 3:
			op.kind = kSLTIU
		case 4:
			op.kind = kXORI
		case 5:
			switch insn >> 26 {
			case 0:
				op.kind, op.imm = kSRLI, op.imm&63
			case 0x10:
				op.kind, op.imm = kSRAI, op.imm&63
			}
		case 6:
			op.kind = kORI
		case 7:
			op.kind = kANDI
		}
	case 0x1b:
		switch {
		case f3 == 0:
			op.kind, op.imm = kADDIW, immI(insn)
		case f3 == 1 && f7 == 0:
			op.kind = kSLLIW
		case f3 == 5 && f7 == 0:
			op.kind = kSRLIW
		case f3 == 5 && f7 == 0x20:
			op.kind = kSRAIW
		}
	case 0x33:
		switch f7 {
		case 0:
			op.kind = [...]uint8{kADD, kSLL, kSLT, kSLTU, kXOR, kSRL, kOR, kAND}[f3]
		case 0x20:
			switch f3 {
			case 0:
				op.kind = kSUB
			case 5:
				op.kind = kSRA
			}
		}
	case 0x3b:
		if f3 == 0 {
			switch f7 {
			case 0:
				op.kind = kADDW
			case 0x20:
				op.kind = kSUBW
			}
		}
	case 0x0f, 0x73:
		op.kind = kExecEnd // fence/fence.i, system and CSR instructions
	}
	return op
}

// runBlock executes up to max instructions of block e starting at pc and
// returns how many it executed (each counts in Cycles, as in Run). It stops
// exactly where the interpreter would next look at something a block can't
// see: after a memory access left an interrupt pending (MMIO side effects),
// so interrupts are taken at the same instruction by both engines.
func (c *CPU) runBlock(e *blockEntry, pc uint64, max int) int {
	ops := c.blocks.ops[e.start : e.start+uint32(e.n)]
	if len(ops) > max {
		ops = ops[:max]
	}
	x := &c.X
	for i := range ops {
		op := &ops[i]
		c.Cycles++
		next := pc + uint64(op.length)
		switch op.kind {
		case kLUI:
			x[op.rd] = op.imm
		case kAUIPC:
			x[op.rd] = pc + op.imm
		case kADDI:
			x[op.rd] = x[op.rs1] + op.imm
		case kSLTI:
			x[op.rd] = b2u(int64(x[op.rs1]) < int64(op.imm))
		case kSLTIU:
			x[op.rd] = b2u(x[op.rs1] < op.imm)
		case kXORI:
			x[op.rd] = x[op.rs1] ^ op.imm
		case kORI:
			x[op.rd] = x[op.rs1] | op.imm
		case kANDI:
			x[op.rd] = x[op.rs1] & op.imm
		case kSLLI:
			x[op.rd] = x[op.rs1] << op.imm
		case kSRLI:
			x[op.rd] = x[op.rs1] >> op.imm
		case kSRAI:
			x[op.rd] = uint64(int64(x[op.rs1]) >> op.imm)
		case kADDIW:
			x[op.rd] = sext32(x[op.rs1] + op.imm)
		case kSLLIW:
			x[op.rd] = sext32(x[op.rs1] << op.rs2)
		case kSRLIW:
			x[op.rd] = sext32(uint64(uint32(x[op.rs1]) >> op.rs2))
		case kSRAIW:
			x[op.rd] = uint64(int64(int32(x[op.rs1]) >> op.rs2))
		case kADD:
			x[op.rd] = x[op.rs1] + x[op.rs2]
		case kSUB:
			x[op.rd] = x[op.rs1] - x[op.rs2]
		case kSLL:
			x[op.rd] = x[op.rs1] << (x[op.rs2] & 63)
		case kSLT:
			x[op.rd] = b2u(int64(x[op.rs1]) < int64(x[op.rs2]))
		case kSLTU:
			x[op.rd] = b2u(x[op.rs1] < x[op.rs2])
		case kXOR:
			x[op.rd] = x[op.rs1] ^ x[op.rs2]
		case kSRL:
			x[op.rd] = x[op.rs1] >> (x[op.rs2] & 63)
		case kSRA:
			x[op.rd] = uint64(int64(x[op.rs1]) >> (x[op.rs2] & 63))
		case kOR:
			x[op.rd] = x[op.rs1] | x[op.rs2]
		case kAND:
			x[op.rd] = x[op.rs1] & x[op.rs2]
		case kADDW:
			x[op.rd] = sext32(uint64(uint32(x[op.rs1]) + uint32(x[op.rs2])))
		case kSUBW:
			x[op.rd] = sext32(uint64(uint32(x[op.rs1]) - uint32(x[op.rs2])))

		case kLB, kLH, kLW, kLD, kLBU, kLHU, kLWU:
			c.PC = pc // a fault reports this instruction
			v, ok := c.load(x[op.rs1]+op.imm, loadSize[op.kind-kLB])
			if !ok {
				return i + 1
			}
			switch op.kind {
			case kLB:
				v = uint64(int64(int8(v)))
			case kLH:
				v = uint64(int64(int16(v)))
			case kLW:
				v = sext32(v)
			}
			x[op.rd] = v
			if c.mip&c.mie != 0 {
				x[0] = 0
				c.PC = next
				return i + 1
			}
		case kSB, kSH, kSW, kSD:
			c.PC = pc
			if !c.store(x[op.rs1]+op.imm, 1<<(op.kind-kSB), x[op.rs2]) {
				return i + 1
			}
			// The store may have hit this very block's code (the
			// interpreter would fetch the new instructions) or raised an
			// interrupt through a device: stop.
			if e.key == 0 || c.mip&c.mie != 0 {
				c.PC = next
				return i + 1
			}

		case kJAL:
			x[op.rd] = next
			x[0] = 0
			c.PC = pc + op.imm
			return i + 1
		case kJALR:
			t := (x[op.rs1] + op.imm) &^ 1
			x[op.rd] = next
			x[0] = 0
			c.PC = t
			return i + 1
		case kBEQ, kBNE, kBLT, kBGE, kBLTU, kBGEU:
			a, b := x[op.rs1], x[op.rs2]
			var taken bool
			switch op.kind {
			case kBEQ:
				taken = a == b
			case kBNE:
				taken = a != b
			case kBLT:
				taken = int64(a) < int64(b)
			case kBGE:
				taken = int64(a) >= int64(b)
			case kBLTU:
				taken = a < b
			default:
				taken = a >= b
			}
			if taken {
				next = pc + op.imm
			}
			c.PC = next
			return i + 1

		default: // kExec, kExecEnd
			c.PC = pc
			c.exec(op.insn, pc, uint64(op.length))
			x[0] = 0
			if c.PC != next || op.kind == kExecEnd || e.key == 0 || c.mip&c.mie != 0 {
				return i + 1 // trap, jump, or state the block can't assume
			}
			pc = next
			continue
		}
		x[0] = 0
		pc = next
	}
	c.PC = pc
	return len(ops)
}

var loadSize = [...]int{1, 2, 4, 8, 1, 2, 4}

// runBlocks is Run for the block engine.
func (c *CPU) runBlocks(n int) int {
	i := 0
	for i < n {
		if c.mip&c.mie != 0 {
			c.WFI = false
			if c.interrupt() {
				i++ // as the interpreter's loop counts it
				continue
			}
		}
		if c.WFI {
			return i
		}
		pc := c.PC
		if pc>>12 == c.fetchTag && pc&1 == 0 {
			if e := c.lookup(c.fetchOff + pc&0xfff); e != nil {
				i += c.runBlock(e, pc, n-i)
				continue
			}
		}
		c.step()
		i++
	}
	return i
}

// step executes one instruction with the interpreter (the body of Run).
func (c *CPU) step() {
	c.Cycles++
	pc := c.PC
	insn, ok := c.fetch(pc)
	if !ok {
		return
	}
	if insn&3 == 3 {
		c.exec(insn, pc, 4)
	} else if e := rvc[uint16(insn)]; e != 0 {
		c.exec(e, pc, 2)
	} else {
		c.exception(ExcIllegal, uint64(uint16(insn)))
	}
	c.X[0] = 0
}
