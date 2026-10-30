package cpu

const (
	mstatusSIE  = 1 << 1
	mstatusMIE  = 1 << 3
	mstatusSPIE = 1 << 5
	mstatusMPIE = 1 << 7
	mstatusSPP  = 1 << 8
	mstatusMPP  = 3 << 11
	mstatusFS   = 3 << 13
	mstatusMPRV = 1 << 17
	mstatusSUM  = 1 << 18
	mstatusMXR  = 1 << 19
	mstatusTVM  = 1 << 20
	mstatusTW   = 1 << 21
	mstatusTSR  = 1 << 22
	mstatusUXL  = 3 << 32
	mstatusSD   = 1 << 63

	mstatusWritable = mstatusSIE | mstatusMIE | mstatusSPIE | mstatusMPIE | mstatusSPP | mstatusMPP |
		mstatusFS | mstatusMPRV | mstatusSUM | mstatusMXR | mstatusTVM | mstatusTW | mstatusTSR
	sstatusMask = mstatusSIE | mstatusSPIE | mstatusSPP | mstatusFS | mstatusSUM | mstatusMXR | mstatusUXL | mstatusSD

	// misa: RV64 with A, C, D, F, I, M, S, U.
	misa = 2<<62 | 1<<0 | 1<<2 | 1<<3 | 1<<5 | 1<<8 | 1<<12 | 1<<18 | 1<<20
)

const (
	csrFflags   = 0x001
	csrFrm      = 0x002
	csrFcsr     = 0x003
	csrCycle    = 0xc00
	csrTime     = 0xc01
	csrInstret  = 0xc02
	csrSstatus  = 0x100
	csrSie      = 0x104
	csrStvec    = 0x105
	csrScounter = 0x106
	csrSenvcfg  = 0x10a
	csrSscratch = 0x140
	csrSepc     = 0x141
	csrScause   = 0x142
	csrStval    = 0x143
	csrSip      = 0x144
	csrSatp     = 0x180
	csrMstatus  = 0x300
	csrMisa     = 0x301
	csrMedeleg  = 0x302
	csrMideleg  = 0x303
	csrMie      = 0x304
	csrMtvec    = 0x305
	csrMcounter = 0x306
	csrMenvcfg  = 0x30a
	csrMscratch = 0x340
	csrMepc     = 0x341
	csrMcause   = 0x342
	csrMtval    = 0x343
	csrMip      = 0x344
	csrMcycle   = 0xb00
	csrMinstret = 0xb02
)

func (c *CPU) fpEnabled() bool { return c.mstatus&mstatusFS != 0 }

func (c *CPU) readCSR(csr uint32) (uint64, bool) {
	switch {
	case csr >= 0xc03 && csr <= 0xc1f, csr >= 0xb03 && csr <= 0xb1f, csr >= 0x323 && csr <= 0x33f, csr == 0x320:
		return 0, true // hpm counters/events, mcountinhibit
	case csr >= 0x3a0 && csr <= 0x3ef:
		return c.pmp[csr-0x3a0], true
	}
	switch csr {
	case csrFflags:
		return c.fflags, c.fpEnabled()
	case csrFrm:
		return c.frm, c.fpEnabled()
	case csrFcsr:
		return c.frm<<5 | c.fflags, c.fpEnabled()
	case csrCycle, csrInstret, csrMcycle, csrMinstret:
		return c.Cycles, true
	case csrTime:
		return c.Time, true
	case csrSstatus:
		return c.status() & sstatusMask, true
	case csrSie:
		return c.mie & c.mideleg, true
	case csrStvec:
		return c.stvec, true
	case csrScounter:
		return c.scounteren, true
	case csrSenvcfg:
		return c.senvcfg, true
	case csrSscratch:
		return c.sscratch, true
	case csrSepc:
		return c.sepc, true
	case csrScause:
		return c.scause, true
	case csrStval:
		return c.stval, true
	case csrSip:
		return c.mip & c.mideleg, true
	case csrSatp:
		if c.Priv == PrivS && c.mstatus&mstatusTVM != 0 {
			return 0, false
		}
		return c.satp, true
	case csrMstatus:
		return c.status(), true
	case csrMisa:
		return misa, true
	case csrMedeleg:
		return c.medeleg, true
	case csrMideleg:
		return c.mideleg, true
	case csrMie:
		return c.mie, true
	case csrMtvec:
		return c.mtvec, true
	case csrMcounter:
		return c.mcounteren, true
	case csrMenvcfg:
		return c.menvcfg, true
	case csrMscratch:
		return c.mscratch, true
	case csrMepc:
		return c.mepc, true
	case csrMcause:
		return c.mcause, true
	case csrMtval:
		return c.mtval, true
	case csrMip:
		return c.mip, true
	case 0xf11, 0xf12, 0xf13, 0xf14, 0xf15: // mvendorid, marchid, mimpid, mhartid, mconfigptr
		return 0, true
	case 0x7a0, 0x7a1, 0x7a2, 0x7a3, 0x7a4, 0x7a5: // trigger module: no triggers
		return 0, true
	}
	return 0, false
}

func (c *CPU) status() uint64 {
	s := c.mstatus
	if s&mstatusFS == mstatusFS {
		s |= mstatusSD
	}
	return s
}

func (c *CPU) writeCSR(csr uint32, v uint64) bool {
	switch {
	case csr >= 0xb03 && csr <= 0xb1f, csr >= 0x323 && csr <= 0x33f, csr == 0x320:
		return true
	case csr >= 0x3a0 && csr <= 0x3ef:
		c.pmp[csr-0x3a0] = v
		return true
	}
	switch csr {
	case csrFflags:
		if !c.fpEnabled() {
			return false
		}
		c.fflags = v & 0x1f
		c.mstatus |= mstatusFS
	case csrFrm:
		if !c.fpEnabled() {
			return false
		}
		c.frm = v & 7
		c.mstatus |= mstatusFS
	case csrFcsr:
		if !c.fpEnabled() {
			return false
		}
		c.fflags, c.frm = v&0x1f, v>>5&7
		c.mstatus |= mstatusFS
	case csrMcycle, csrMinstret:
		c.Cycles = v - 1 // the writing instruction itself does not count
	case csrSstatus:
		c.setStatus(c.mstatus&^sstatusMask | v&sstatusMask)
	case csrSie:
		c.mie = c.mie&^c.mideleg | v&c.mideleg
	case csrStvec:
		c.stvec = v &^ 2
	case csrScounter:
		c.scounteren = v & 0xffffffff
	case csrSenvcfg:
		c.senvcfg = v & 1 // FIOM
	case csrSscratch:
		c.sscratch = v
	case csrSepc:
		c.sepc = v &^ 1
	case csrScause:
		c.scause = v
	case csrStval:
		c.stval = v
	case csrSip:
		mask := c.mideleg & IntSSI
		c.mip = c.mip&^mask | v&mask
	case csrSatp:
		if c.Priv == PrivS && c.mstatus&mstatusTVM != 0 {
			return false
		}
		// Only Bare and Sv39 are supported; other modes leave satp unchanged
		// (WARL), which is how Linux probes for Sv48/Sv57. ASIDs are not
		// implemented, so ASID bits read as zero.
		if mode := v >> 60; mode == 0 || mode == 8 {
			c.satp = v & (0xf<<60 | (1<<44 - 1))
			c.flushTLB()
			c.updateMode()
		}
	case csrMstatus:
		c.setStatus(c.mstatus&^mstatusWritable | v&mstatusWritable)
	case csrMisa:
	case csrMedeleg:
		c.medeleg = v & 0xb3ff
	case csrMideleg:
		c.mideleg = v & (IntSSI | IntSTI | IntSEI)
	case csrMie:
		c.mie = v & 0xaaa
	case csrMtvec:
		c.mtvec = v &^ 2
	case csrMcounter:
		c.mcounteren = v & 0xffffffff
	case csrMenvcfg:
		c.menvcfg = v & 1
	case csrMscratch:
		c.mscratch = v
	case csrMepc:
		c.mepc = v &^ 1
	case csrMcause:
		c.mcause = v
	case csrMtval:
		c.mtval = v
	case csrMip:
		mask := uint64(IntSSI | IntSTI | IntSEI)
		c.mip = c.mip&^mask | v&mask
	case 0x7a0, 0x7a1, 0x7a2, 0x7a3, 0x7a4, 0x7a5:
	default:
		return false
	}
	return true
}

func (c *CPU) setStatus(v uint64) {
	if v&mstatusMPP == 2<<11 { // reserved encoding: keep old MPP
		v = v&^mstatusMPP | c.mstatus&mstatusMPP
	}
	c.mstatus = v
	c.updateMode()
}

// execCSR implements the Zicsr instructions.
func (c *CPU) execCSR(insn uint32, f3 uint32, rd, rs1 uint32) bool {
	csr := insn >> 20
	if uint32(c.Priv) < csr>>8&3 {
		return false
	}
	src := c.X[rs1]
	if f3 >= 5 {
		src = uint64(rs1) // zimm
	}
	write := true
	switch f3 & 3 {
	case 2, 3: // csrrs, csrrc with x0/zero leave the CSR untouched
		write = rs1 != 0
	}
	if write && csr>>10 == 3 {
		return false
	}
	var old uint64
	if f3&3 != 1 || rd != 0 {
		v, ok := c.readCSR(csr)
		if !ok {
			return false
		}
		old = v
	} else if _, ok := c.readCSR(csr); !ok {
		return false
	}
	if write {
		nv := src
		switch f3 & 3 {
		case 2:
			nv = old | src
		case 3:
			nv = old &^ src
		}
		if !c.writeCSR(csr, nv) {
			return false
		}
	}
	c.X[rd] = old
	return true
}
