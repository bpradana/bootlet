package cpu

import "math/bits"

func immI(insn uint32) uint64 { return uint64(int64(int32(insn) >> 20)) }
func immS(insn uint32) uint64 {
	return uint64(int64(int32(insn&0xfe000000)>>20) | int64(insn>>7&0x1f))
}
func immB(insn uint32) uint64 {
	return uint64(int64(int32(insn&0x80000000)>>19) | int64(insn&0x80)<<4 |
		int64(insn>>20&0x7e0) | int64(insn>>7&0x1e))
}
func immU(insn uint32) uint64 { return uint64(int64(int32(insn & 0xfffff000))) }
func immJ(insn uint32) uint64 {
	return uint64(int64(int32(insn&0x80000000)>>11) | int64(insn&0xff000) |
		int64(insn>>9&0x800) | int64(insn>>20&0x7fe))
}

func sext32(v uint64) uint64 { return uint64(int64(int32(v))) }

// exec executes one (expanded) 32-bit instruction located at pc with the
// given encoded length.
func (c *CPU) exec(insn uint32, pc uint64, length uint64) {
	rd := insn >> 7 & 0x1f
	rs1 := insn >> 15 & 0x1f
	rs2 := insn >> 20 & 0x1f
	f3 := insn >> 12 & 7
	next := pc + length

	// Dispatch on opcode[6:2]: a dense 0..31 range compiles to a jump table.
	// (Bits [1:0] are always 11 here; compressed instructions are expanded.)
	switch insn >> 2 & 0x1f {
	case 0x37 >> 2: // lui
		c.X[rd] = immU(insn)
	case 0x17 >> 2: // auipc
		c.X[rd] = pc + immU(insn)
	case 0x6f >> 2: // jal
		c.X[rd] = next
		next = pc + immJ(insn)
	case 0x67 >> 2: // jalr
		t := (c.X[rs1] + immI(insn)) &^ 1
		c.X[rd] = next
		next = t
	case 0x63 >> 2: // branches
		a, b := c.X[rs1], c.X[rs2]
		var taken bool
		switch f3 {
		case 0:
			taken = a == b
		case 1:
			taken = a != b
		case 4:
			taken = int64(a) < int64(b)
		case 5:
			taken = int64(a) >= int64(b)
		case 6:
			taken = a < b
		case 7:
			taken = a >= b
		default:
			c.illegal(insn, length)
			return
		}
		if taken {
			next = pc + immB(insn)
		}
	case 0x03 >> 2: // loads
		addr := c.X[rs1] + immI(insn)
		var v uint64
		var ok bool
		switch f3 {
		case 0:
			v, ok = c.load(addr, 1)
			v = uint64(int64(int8(v)))
		case 1:
			v, ok = c.load(addr, 2)
			v = uint64(int64(int16(v)))
		case 2:
			v, ok = c.load(addr, 4)
			v = sext32(v)
		case 3:
			v, ok = c.load(addr, 8)
		case 4:
			v, ok = c.load(addr, 1)
		case 5:
			v, ok = c.load(addr, 2)
		case 6:
			v, ok = c.load(addr, 4)
		default:
			c.illegal(insn, length)
			return
		}
		if !ok {
			return
		}
		c.X[rd] = v
	case 0x23 >> 2: // stores
		if f3 > 3 {
			c.illegal(insn, length)
			return
		}
		if !c.store(c.X[rs1]+immS(insn), 1<<f3, c.X[rs2]) {
			return
		}
	case 0x13 >> 2: // op-imm
		a, imm := c.X[rs1], immI(insn)
		switch f3 {
		case 0:
			c.X[rd] = a + imm
		case 1:
			if insn>>26 != 0 {
				c.illegal(insn, length)
				return
			}
			c.X[rd] = a << (imm & 63)
		case 2:
			c.X[rd] = b2u(int64(a) < int64(imm))
		case 3:
			c.X[rd] = b2u(a < imm)
		case 4:
			c.X[rd] = a ^ imm
		case 5:
			switch insn >> 26 {
			case 0:
				c.X[rd] = a >> (imm & 63)
			case 0x10:
				c.X[rd] = uint64(int64(a) >> (imm & 63))
			default:
				c.illegal(insn, length)
				return
			}
		case 6:
			c.X[rd] = a | imm
		case 7:
			c.X[rd] = a & imm
		}
	case 0x1b >> 2: // op-imm-32
		a := c.X[rs1]
		switch f3 {
		case 0:
			c.X[rd] = sext32(a + immI(insn))
		case 1:
			if insn>>25 != 0 {
				c.illegal(insn, length)
				return
			}
			c.X[rd] = sext32(a << rs2)
		case 5:
			switch insn >> 25 {
			case 0:
				c.X[rd] = sext32(uint64(uint32(a) >> rs2))
			case 0x20:
				c.X[rd] = uint64(int64(int32(a) >> rs2))
			default:
				c.illegal(insn, length)
				return
			}
		default:
			c.illegal(insn, length)
			return
		}
	case 0x33 >> 2: // op
		a, b := c.X[rs1], c.X[rs2]
		if insn>>25 == 0 { // base integer ops, inline for speed
			switch f3 {
			case 0:
				c.X[rd] = a + b
			case 1:
				c.X[rd] = a << (b & 63)
			case 2:
				c.X[rd] = b2u(int64(a) < int64(b))
			case 3:
				c.X[rd] = b2u(a < b)
			case 4:
				c.X[rd] = a ^ b
			case 5:
				c.X[rd] = a >> (b & 63)
			case 6:
				c.X[rd] = a | b
			case 7:
				c.X[rd] = a & b
			}
		} else if !c.op(insn, rd, a, b, f3) {
			c.illegal(insn, length)
			return
		}
	case 0x3b >> 2: // op-32
		if !c.op32(insn, rd, c.X[rs1], c.X[rs2], f3) {
			c.illegal(insn, length)
			return
		}
	case 0x0f >> 2: // fence, fence.i
		if f3 > 1 {
			c.illegal(insn, length)
			return
		}
	case 0x73 >> 2:
		if !c.system(insn, pc, &next, rd, rs1, f3) {
			if c.PC == pc { // not already trapped
				c.illegal(insn, length)
			}
			return
		}
	case 0x2f >> 2:
		ok, trapped := c.amo(insn, rd, rs1, rs2, f3)
		if trapped {
			return
		}
		if !ok {
			c.illegal(insn, length)
			return
		}
	case 0x07 >> 2, 0x27 >> 2, 0x43 >> 2, 0x47 >> 2, 0x4b >> 2, 0x4f >> 2, 0x53 >> 2:
		ok, trapped := c.fp(insn, rd, rs1, rs2, f3)
		if trapped {
			return
		}
		if !ok {
			c.illegal(insn, length)
			return
		}
	default:
		c.illegal(insn, length)
		return
	}
	c.PC = next
}

func (c *CPU) illegal(insn uint32, length uint64) {
	if length == 2 {
		insn = 0 // the original compressed encoding is not tracked
	}
	c.exception(ExcIllegal, uint64(insn))
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// op executes the OP instructions other than the base ones handled inline
// in exec: sub/sra and the M extension.
func (c *CPU) op(insn, rd uint32, a, b uint64, f3 uint32) bool {
	switch insn >> 25 {
	case 0x20:
		switch f3 {
		case 0:
			c.X[rd] = a - b
		case 5:
			c.X[rd] = uint64(int64(a) >> (b & 63))
		default:
			return false
		}
	case 1: // M extension
		switch f3 {
		case 0:
			c.X[rd] = a * b
		case 1:
			c.X[rd] = mulh(int64(a), int64(b))
		case 2:
			c.X[rd] = mulhsu(int64(a), b)
		case 3:
			c.X[rd], _ = bits.Mul64(a, b)
		case 4:
			switch {
			case b == 0:
				c.X[rd] = ^uint64(0)
			case int64(a) == -1<<63 && int64(b) == -1:
				c.X[rd] = a
			default:
				c.X[rd] = uint64(int64(a) / int64(b))
			}
		case 5:
			if b == 0 {
				c.X[rd] = ^uint64(0)
			} else {
				c.X[rd] = a / b
			}
		case 6:
			switch {
			case b == 0:
				c.X[rd] = a
			case int64(a) == -1<<63 && int64(b) == -1:
				c.X[rd] = 0
			default:
				c.X[rd] = uint64(int64(a) % int64(b))
			}
		case 7:
			if b == 0 {
				c.X[rd] = a
			} else {
				c.X[rd] = a % b
			}
		}
	default:
		return false
	}
	return true
}

func mulh(a, b int64) uint64 {
	hi, _ := bits.Mul64(uint64(a), uint64(b))
	if a < 0 {
		hi -= uint64(b)
	}
	if b < 0 {
		hi -= uint64(a)
	}
	return hi
}

func mulhsu(a int64, b uint64) uint64 {
	hi, _ := bits.Mul64(uint64(a), b)
	if a < 0 {
		hi -= b
	}
	return hi
}

func (c *CPU) op32(insn, rd uint32, a, b uint64, f3 uint32) bool {
	x, y := uint32(a), uint32(b)
	switch insn >> 25 {
	case 0:
		switch f3 {
		case 0:
			c.X[rd] = sext32(uint64(x + y))
		case 1:
			c.X[rd] = sext32(uint64(x << (y & 31)))
		case 5:
			c.X[rd] = sext32(uint64(x >> (y & 31)))
		default:
			return false
		}
	case 0x20:
		switch f3 {
		case 0:
			c.X[rd] = sext32(uint64(x - y))
		case 5:
			c.X[rd] = uint64(int64(int32(x) >> (y & 31)))
		default:
			return false
		}
	case 1:
		switch f3 {
		case 0:
			c.X[rd] = sext32(uint64(x * y))
		case 4:
			switch {
			case y == 0:
				c.X[rd] = ^uint64(0)
			case int32(x) == -1<<31 && int32(y) == -1:
				c.X[rd] = sext32(uint64(x))
			default:
				c.X[rd] = uint64(int64(int32(x) / int32(y)))
			}
		case 5:
			if y == 0 {
				c.X[rd] = ^uint64(0)
			} else {
				c.X[rd] = sext32(uint64(x / y))
			}
		case 6:
			switch {
			case y == 0:
				c.X[rd] = sext32(uint64(x))
			case int32(x) == -1<<31 && int32(y) == -1:
				c.X[rd] = 0
			default:
				c.X[rd] = uint64(int64(int32(x) % int32(y)))
			}
		case 7:
			if y == 0 {
				c.X[rd] = sext32(uint64(x))
			} else {
				c.X[rd] = sext32(uint64(x % y))
			}
		default:
			return false
		}
	default:
		return false
	}
	return true
}

// system handles ecall/ebreak/xret/wfi/sfence.vma and the CSR instructions.
// It returns false for an illegal instruction (unless a trap was already
// taken, which the caller detects by PC having changed).
func (c *CPU) system(insn uint32, pc uint64, next *uint64, rd, rs1, f3 uint32) bool {
	if f3 != 0 {
		if f3 == 4 {
			return false
		}
		return c.execCSR(insn, f3, rd, rs1)
	}
	switch {
	case insn == 0x00000073: // ecall
		if c.Priv == PrivS && c.Ecall != nil {
			c.Ecall(c)
			return true
		}
		c.exception([...]uint64{ExcEcallU, ExcEcallS, 0, ExcEcallM}[c.Priv], 0)
		return false
	case insn == 0x00100073: // ebreak
		c.exception(ExcBreakpoint, pc)
		return false
	case insn == 0x10200073: // sret
		if c.Priv < PrivS || (c.Priv == PrivS && c.mstatus&mstatusTSR != 0) {
			return false
		}
		s := c.mstatus
		c.Priv = uint8(s >> 8 & 1)
		s &^= mstatusSIE | mstatusSPP
		if s&mstatusSPIE != 0 {
			s |= mstatusSIE
		}
		s |= mstatusSPIE
		if c.Priv != PrivM {
			s &^= mstatusMPRV
		}
		c.mstatus = s
		*next = c.sepc
		c.updateMode()
	case insn == 0x30200073: // mret
		if c.Priv < PrivM {
			return false
		}
		s := c.mstatus
		c.Priv = uint8(s >> 11 & 3)
		s &^= mstatusMIE | mstatusMPP
		if s&mstatusMPIE != 0 {
			s |= mstatusMIE
		}
		s |= mstatusMPIE
		if c.Priv != PrivM {
			s &^= mstatusMPRV
		}
		c.mstatus = s
		*next = c.mepc
		c.updateMode()
	case insn == 0x10500073: // wfi
		if c.Priv < PrivM && c.mstatus&mstatusTW != 0 || c.Priv == PrivU {
			return false
		}
		c.WFI = c.mip&c.mie == 0
	case insn>>25 == 0x09 && rd == 0: // sfence.vma
		if c.Priv < PrivS || (c.Priv == PrivS && c.mstatus&mstatusTVM != 0) {
			return false
		}
		c.flushTLB()
	default:
		return false
	}
	return true
}

// amo implements the A extension. It reports whether the encoding was valid
// and whether a trap was taken.
func (c *CPU) amo(insn, rd, rs1, rs2, f3 uint32) (ok, trapped bool) {
	size := 4
	switch f3 {
	case 2:
	case 3:
		size = 8
	default:
		return false, false
	}
	addr := c.X[rs1]
	f5 := insn >> 27
	if addr&uint64(size-1) != 0 {
		if f5 == 2 {
			c.exception(ExcLoadMisaligned, addr)
		} else {
			c.exception(ExcStoreMisaligned, addr)
		}
		return true, true
	}
	ext := func(v uint64) uint64 {
		if size == 4 {
			return sext32(v)
		}
		return v
	}
	switch f5 {
	case 2: // lr
		if rs2 != 0 {
			return false, false
		}
		v, ok := c.load(addr, size)
		if !ok {
			return true, true
		}
		c.X[rd] = ext(v)
		c.reservation, c.reservationValid = addr, true
		return true, false
	case 3: // sc
		if c.reservationValid && c.reservation == addr {
			if !c.store(addr, size, c.X[rs2]) {
				return true, true
			}
			c.X[rd] = 0
		} else {
			c.X[rd] = 1
		}
		c.reservationValid = false
		return true, false
	}
	// Probe for write permission first so a read-only page reports a store fault.
	if _, ok := c.translate(addr, accStore); !ok {
		return true, true
	}
	old, ok := c.load(addr, size)
	if !ok {
		return true, true
	}
	old = ext(old)
	src := ext(c.X[rs2])
	var v uint64
	switch f5 {
	case 0x01: // amoswap
		v = src
	case 0x00: // amoadd
		v = old + src
	case 0x04: // amoxor
		v = old ^ src
	case 0x0c: // amoand
		v = old & src
	case 0x08: // amoor
		v = old | src
	case 0x10: // amomin
		v = old
		if int64(src) < int64(old) {
			v = src
		}
	case 0x14: // amomax
		v = old
		if int64(src) > int64(old) {
			v = src
		}
	case 0x18: // amominu
		v = old
		if src < old {
			v = src
		}
	case 0x1c: // amomaxu
		v = old
		if src > old {
			v = src
		}
	default:
		return false, false
	}
	if !c.store(addr, size, v) {
		return true, true
	}
	c.X[rd] = old
	return true, false
}
