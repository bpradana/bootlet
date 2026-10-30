// Package cpu implements a single RV64GC hart (RV64IMAFDC + Zicsr, Zifencei,
// Zicntr) with M/S/U privilege modes and Sv39 virtual memory.
//
// Errors inside instruction execution are handled by taking the trap
// immediately (updating PC and the trap CSRs) and returning false, so the
// hot path never allocates or panics.
package cpu

import "encoding/binary"

const (
	PrivU = 0
	PrivS = 1
	PrivM = 3
)

// Bus is the MMIO interface for physical addresses outside RAM.
// ok=false signals an access fault.
type Bus interface {
	Read(addr uint64, size int) (val uint64, ok bool)
	Write(addr uint64, size int, val uint64) (ok bool)
}

// Interrupt bits in mip/mie.
const (
	IntSSI = 1 << 1
	IntMSI = 1 << 3
	IntSTI = 1 << 5
	IntMTI = 1 << 7
	IntSEI = 1 << 9
	IntMEI = 1 << 11
)

// Exception causes.
const (
	ExcInsnMisaligned  = 0
	ExcInsnAccess      = 1
	ExcIllegal         = 2
	ExcBreakpoint      = 3
	ExcLoadMisaligned  = 4
	ExcLoadAccess      = 5
	ExcStoreMisaligned = 6
	ExcStoreAccess     = 7
	ExcEcallU          = 8
	ExcEcallS          = 9
	ExcEcallM          = 11
	ExcInsnPage        = 12
	ExcLoadPage        = 13
	ExcStorePage       = 15
)

type CPU struct {
	X    [32]uint64
	F    [32]uint64
	PC   uint64
	Priv uint8

	// Time is the value of the time CSR (and CLINT mtime), maintained by the
	// owner of the CPU.
	Time   uint64
	Cycles uint64

	RAM     []byte
	RAMBase uint64
	Bus     Bus

	// Ecall, if set, is called for ecall from S-mode instead of trapping to
	// M-mode. It implements the SBI firmware in Go.
	Ecall func(c *CPU)

	// WFI is set when the hart executed wfi and no interrupt is pending.
	WFI bool

	mstatus    uint64
	medeleg    uint64
	mideleg    uint64
	mie        uint64
	mip        uint64
	mtvec      uint64
	mscratch   uint64
	mepc       uint64
	mcause     uint64
	mtval      uint64
	mcounteren uint64
	menvcfg    uint64
	stvec      uint64
	sscratch   uint64
	sepc       uint64
	scause     uint64
	stval      uint64
	satp       uint64
	scounteren uint64
	senvcfg    uint64
	fflags     uint64
	frm        uint64
	pmp        [80]uint64 // pmpcfg0..15 + pmpaddr0..63, stored but not enforced

	reservation      uint64
	reservationValid bool

	// Cached translation state, recomputed by updateMode.
	fetchPaging bool
	dataPaging  bool
	dataPriv    uint8

	// Instruction fetch page cache: host RAM offset of the current page.
	fetchTag uint64
	fetchOff uint64

	itlb [tlbSize]tlbEntry
	dtlb [tlbSize]tlbEntry
}

func New(ram []byte, ramBase uint64, bus Bus) *CPU {
	c := &CPU{RAM: ram, RAMBase: ramBase, Bus: bus}
	c.Reset(ramBase)
	return c
}

// Reset puts the hart in M-mode at pc with all state cleared.
func (c *CPU) Reset(pc uint64) {
	ram, base, bus, ecall := c.RAM, c.RAMBase, c.Bus, c.Ecall
	*c = CPU{RAM: ram, RAMBase: base, Bus: bus, Ecall: ecall}
	c.PC = pc
	c.Priv = PrivM
	c.mstatus = 2<<32 | 2<<34 // UXL = SXL = 64
	c.updateMode()
	c.flushTLB()
}

// SetPending raises or lowers interrupt-pending bits driven by devices.
func (c *CPU) SetPending(mask uint64, on bool) {
	if on {
		c.mip |= mask
	} else {
		c.mip &^= mask
	}
}

// Pending reports mip.
func (c *CPU) Pending() uint64 { return c.mip }

// Enabled reports mie.
func (c *CPU) Enabled() uint64 { return c.mie }

// EnterSupervisor configures the M-mode state that firmware would normally
// leave behind (delegation, counters) and drops to S-mode at pc.
func (c *CPU) EnterSupervisor(pc uint64) {
	// Delegate every exception the kernel can handle; ecall from S goes to Ecall.
	c.medeleg = 1<<ExcInsnMisaligned | 1<<ExcInsnAccess | 1<<ExcIllegal | 1<<ExcBreakpoint |
		1<<ExcLoadMisaligned | 1<<ExcLoadAccess | 1<<ExcStoreMisaligned | 1<<ExcStoreAccess |
		1<<ExcEcallU | 1<<ExcInsnPage | 1<<ExcLoadPage | 1<<ExcStorePage
	c.mideleg = IntSSI | IntSTI | IntSEI
	c.mcounteren = 7
	c.scounteren = 7
	c.Priv = PrivS
	c.PC = pc
	c.updateMode()
}

// Run executes up to n instructions. It returns early when the hart enters
// wfi with nothing pending. It returns the number of instructions executed.
//
// The instruction fetch fast path is inlined here: function calls are
// comparatively expensive under WebAssembly, so the common case of fetching
// from the current page costs no call at all.
func (c *CPU) Run(n int) int {
	for i := 0; i < n; i++ {
		if c.mip&c.mie != 0 {
			c.WFI = false
			if c.interrupt() {
				continue
			}
		}
		if c.WFI {
			return i
		}
		c.Cycles++
		pc := c.PC
		var insn uint32
		if pc>>12 == c.fetchTag && pc&0xfff <= 0xffc {
			insn = binary.LittleEndian.Uint32(c.RAM[c.fetchOff+pc&0xfff:])
		} else {
			var ok bool
			if insn, ok = c.fetch(pc); !ok {
				continue
			}
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
	return n
}

// interrupt takes the highest-priority pending and enabled interrupt, if any.
func (c *CPU) interrupt() bool {
	pending := c.mip & c.mie
	mPending := pending &^ c.mideleg
	if c.Priv < PrivM || c.mstatus&mstatusMIE != 0 {
		if mPending != 0 {
			c.trap(true, pickInterrupt(mPending), 0)
			return true
		}
	}
	sPending := pending & c.mideleg
	if c.Priv < PrivS || (c.Priv == PrivS && c.mstatus&mstatusSIE != 0) {
		if sPending != 0 {
			c.trap(true, pickInterrupt(sPending), 0)
			return true
		}
	}
	return false
}

func pickInterrupt(p uint64) uint64 {
	for _, n := range [...]uint64{11, 3, 7, 9, 1, 5} {
		if p&(1<<n) != 0 {
			return n
		}
	}
	return 0
}

func (c *CPU) exception(cause, tval uint64) {
	c.trap(false, cause, tval)
}

func (c *CPU) trap(isInterrupt bool, cause, tval uint64) {
	c.reservationValid = false
	deleg := c.medeleg
	code := cause
	if isInterrupt {
		deleg = c.mideleg
		code |= 1 << 63
	}
	if c.Priv <= PrivS && deleg&(1<<cause) != 0 {
		c.scause = code
		c.sepc = c.PC
		c.stval = tval
		s := c.mstatus &^ (mstatusSPIE | mstatusSPP | mstatusSIE)
		if c.mstatus&mstatusSIE != 0 {
			s |= mstatusSPIE
		}
		if c.Priv == PrivS {
			s |= mstatusSPP
		}
		c.mstatus = s
		c.Priv = PrivS
		c.PC = trapVector(c.stvec, isInterrupt, cause)
	} else {
		c.mcause = code
		c.mepc = c.PC
		c.mtval = tval
		s := c.mstatus &^ (mstatusMPIE | mstatusMPP | mstatusMIE)
		if c.mstatus&mstatusMIE != 0 {
			s |= mstatusMPIE
		}
		s |= uint64(c.Priv) << 11
		c.mstatus = s
		c.Priv = PrivM
		c.PC = trapVector(c.mtvec, isInterrupt, cause)
	}
	c.updateMode()
}

func trapVector(tvec uint64, isInterrupt bool, cause uint64) uint64 {
	base := tvec &^ 3
	if isInterrupt && tvec&3 == 1 {
		return base + 4*cause
	}
	return base
}

// updateMode recomputes cached translation state after a change to the
// privilege level, mstatus or satp.
func (c *CPU) updateMode() {
	sv39 := c.satp>>60 == 8
	c.fetchPaging = sv39 && c.Priv < PrivM
	c.dataPriv = c.Priv
	if c.Priv == PrivM && c.mstatus&mstatusMPRV != 0 {
		c.dataPriv = uint8(c.mstatus>>11) & 3
	}
	c.dataPaging = sv39 && c.dataPriv < PrivM
	c.fetchTag = noTag
}
