package cpu

// rvc maps every 16-bit compressed instruction to its 32-bit equivalent
// (0 for illegal/reserved encodings), so decoding a compressed instruction is
// a single table load.
var rvc [1 << 16]uint32

func init() {
	for i := range rvc {
		if i&3 != 3 {
			rvc[i] = expand(uint16(i))
		}
	}
}

func encR(op, rd, f3, rs1, rs2, f7 uint32) uint32 {
	return f7<<25 | rs2<<20 | rs1<<15 | f3<<12 | rd<<7 | op
}

func encI(op, rd, f3, rs1 uint32, imm int32) uint32 {
	return uint32(imm)<<20 | rs1<<15 | f3<<12 | rd<<7 | op
}

func encS(op, f3, rs1, rs2 uint32, imm int32) uint32 {
	u := uint32(imm)
	return (u>>5&0x7f)<<25 | rs2<<20 | rs1<<15 | f3<<12 | (u&0x1f)<<7 | op
}

func encB(f3, rs1, rs2 uint32, imm int32) uint32 {
	u := uint32(imm)
	return (u>>12&1)<<31 | (u>>5&0x3f)<<25 | rs2<<20 | rs1<<15 | f3<<12 |
		(u>>1&0xf)<<8 | (u>>11&1)<<7 | 0x63
}

func encJ(rd uint32, imm int32) uint32 {
	u := uint32(imm)
	return (u>>20&1)<<31 | (u>>1&0x3ff)<<21 | (u>>11&1)<<20 | (u>>12&0xff)<<12 | rd<<7 | 0x6f
}

func bit(x uint16, n uint) uint32 { return uint32(x>>n) & 1 }

func bitsOf(x uint16, hi, lo uint) uint32 { return uint32(x>>lo) & (1<<(hi-lo+1) - 1) }

func sx(v uint32, width uint) int32 { return int32(v<<(32-width)) >> (32 - width) }

func expand(h uint16) uint32 {
	rd := bitsOf(h, 11, 7)
	rs2 := bitsOf(h, 6, 2)
	rdp := 8 + bitsOf(h, 4, 2)  // rd'/rs2'
	rs1p := 8 + bitsOf(h, 9, 7) // rs1'
	f3 := bitsOf(h, 15, 13)
	imm6 := sx(bit(h, 12)<<5|bitsOf(h, 6, 2), 6)

	switch h & 3 {
	case 0:
		switch f3 {
		case 0: // c.addi4spn
			imm := bitsOf(h, 12, 11)<<4 | bitsOf(h, 10, 7)<<6 | bit(h, 6)<<2 | bit(h, 5)<<3
			if imm == 0 {
				return 0
			}
			return encI(0x13, rdp, 0, 2, int32(imm))
		case 1, 3: // c.fld, c.ld
			imm := bitsOf(h, 12, 10)<<3 | bitsOf(h, 6, 5)<<6
			return encI([2]uint32{0x07, 0x03}[f3>>1], rdp, 3, rs1p, int32(imm))
		case 2: // c.lw
			imm := bitsOf(h, 12, 10)<<3 | bit(h, 6)<<2 | bit(h, 5)<<6
			return encI(0x03, rdp, 2, rs1p, int32(imm))
		case 5, 7: // c.fsd, c.sd
			imm := bitsOf(h, 12, 10)<<3 | bitsOf(h, 6, 5)<<6
			return encS([2]uint32{0x27, 0x23}[f3>>1&1], 3, rs1p, rdp, int32(imm))
		case 6: // c.sw
			imm := bitsOf(h, 12, 10)<<3 | bit(h, 6)<<2 | bit(h, 5)<<6
			return encS(0x23, 2, rs1p, rdp, int32(imm))
		}
	case 1:
		switch f3 {
		case 0: // c.addi / c.nop
			return encI(0x13, rd, 0, rd, imm6)
		case 1: // c.addiw
			if rd == 0 {
				return 0
			}
			return encI(0x1b, rd, 0, rd, imm6)
		case 2: // c.li
			return encI(0x13, rd, 0, 0, imm6)
		case 3:
			if rd == 2 { // c.addi16sp
				imm := sx(bit(h, 12)<<9|bit(h, 6)<<4|bit(h, 5)<<6|bitsOf(h, 4, 3)<<7|bit(h, 2)<<5, 10)
				if imm == 0 {
					return 0
				}
				return encI(0x13, 2, 0, 2, imm)
			}
			if imm6 == 0 { // c.lui
				return 0
			}
			return uint32(imm6)<<12 | rd<<7 | 0x37
		case 4:
			switch bitsOf(h, 11, 10) {
			case 0: // c.srli
				return encI(0x13, rs1p, 5, rs1p, int32(bit(h, 12)<<5|bitsOf(h, 6, 2)))
			case 1: // c.srai
				return encI(0x13, rs1p, 5, rs1p, int32(0x400|bit(h, 12)<<5|bitsOf(h, 6, 2)))
			case 2: // c.andi
				return encI(0x13, rs1p, 7, rs1p, imm6)
			}
			op := bitsOf(h, 6, 5)
			if bit(h, 12) == 0 {
				switch op {
				case 0:
					return encR(0x33, rs1p, 0, rs1p, rdp, 0x20) // c.sub
				case 1:
					return encR(0x33, rs1p, 4, rs1p, rdp, 0) // c.xor
				case 2:
					return encR(0x33, rs1p, 6, rs1p, rdp, 0) // c.or
				default:
					return encR(0x33, rs1p, 7, rs1p, rdp, 0) // c.and
				}
			}
			switch op {
			case 0:
				return encR(0x3b, rs1p, 0, rs1p, rdp, 0x20) // c.subw
			case 1:
				return encR(0x3b, rs1p, 0, rs1p, rdp, 0) // c.addw
			}
			return 0
		case 5: // c.j
			imm := sx(bit(h, 12)<<11|bit(h, 11)<<4|bitsOf(h, 10, 9)<<8|bit(h, 8)<<10|bit(h, 7)<<6|
				bit(h, 6)<<7|bitsOf(h, 5, 3)<<1|bit(h, 2)<<5, 12)
			return encJ(0, imm)
		case 6, 7: // c.beqz, c.bnez
			imm := sx(bit(h, 12)<<8|bitsOf(h, 11, 10)<<3|bitsOf(h, 6, 5)<<6|bitsOf(h, 4, 3)<<1|bit(h, 2)<<5, 9)
			return encB(f3-6, rs1p, 0, imm)
		}
	case 2:
		switch f3 {
		case 0: // c.slli
			return encI(0x13, rd, 1, rd, int32(bit(h, 12)<<5|bitsOf(h, 6, 2)))
		case 1, 3: // c.fldsp, c.ldsp
			if f3 == 3 && rd == 0 {
				return 0
			}
			imm := bit(h, 12)<<5 | bitsOf(h, 6, 5)<<3 | bitsOf(h, 4, 2)<<6
			return encI([2]uint32{0x07, 0x03}[f3>>1], rd, 3, 2, int32(imm))
		case 2: // c.lwsp
			if rd == 0 {
				return 0
			}
			imm := bit(h, 12)<<5 | bitsOf(h, 6, 4)<<2 | bitsOf(h, 3, 2)<<6
			return encI(0x03, rd, 2, 2, int32(imm))
		case 4:
			if bit(h, 12) == 0 {
				if rs2 == 0 { // c.jr
					if rd == 0 {
						return 0
					}
					return encI(0x67, 0, 0, rd, 0)
				}
				return encR(0x33, rd, 0, 0, rs2, 0) // c.mv
			}
			if rs2 == 0 {
				if rd == 0 {
					return 0x00100073 // c.ebreak
				}
				return encI(0x67, 1, 0, rd, 0) // c.jalr
			}
			return encR(0x33, rd, 0, rd, rs2, 0) // c.add
		case 5, 7: // c.fsdsp, c.sdsp
			imm := bitsOf(h, 12, 10)<<3 | bitsOf(h, 9, 7)<<6
			return encS([2]uint32{0x27, 0x23}[f3>>1&1], 3, 2, rs2, int32(imm))
		case 6: // c.swsp
			imm := bitsOf(h, 12, 9)<<2 | bitsOf(h, 8, 7)<<6
			return encS(0x23, 2, 2, rs2, int32(imm))
		}
	}
	return 0
}
