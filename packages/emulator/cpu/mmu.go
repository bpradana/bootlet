package cpu

import "encoding/binary"

type access uint8

const (
	accFetch access = iota
	accLoad
	accStore
)

const (
	pteV = 1 << 0
	pteR = 1 << 1
	pteW = 1 << 2
	pteX = 1 << 3
	pteU = 1 << 4
	pteA = 1 << 6
	pteD = 1 << 7
)

const (
	tlbSize = 256
	noTag   = ^uint64(0)
)

// tlbEntry caches one 4 KiB translation. flags are the leaf PTE bits; the
// permission check is repeated on every hit so the TLB survives privilege and
// SUM/MXR changes and only needs flushing on satp writes and sfence.vma.
type tlbEntry struct {
	tag   uint64 // virtual page number, or noTag
	page  uint64 // physical page address
	flags uint8
}

func (c *CPU) flushTLB() {
	for i := range c.itlb {
		c.itlb[i].tag = noTag
		c.dtlb[i].tag = noTag
	}
	c.fetchTag = noTag
}

var faultCause = [...]uint64{accFetch: ExcInsnPage, accLoad: ExcLoadPage, accStore: ExcStorePage}
var accessCause = [...]uint64{accFetch: ExcInsnAccess, accLoad: ExcLoadAccess, accStore: ExcStoreAccess}

func (c *CPU) permitted(flags uint8, acc access, priv uint8) bool {
	switch acc {
	case accFetch:
		if flags&pteX == 0 {
			return false
		}
	case accLoad:
		if flags&pteR == 0 && (c.mstatus&mstatusMXR == 0 || flags&pteX == 0) {
			return false
		}
	case accStore:
		if flags&pteW == 0 || flags&pteD == 0 {
			return false
		}
	}
	if priv == PrivU {
		return flags&pteU != 0
	}
	if flags&pteU != 0 {
		return acc != accFetch && c.mstatus&mstatusSUM != 0
	}
	return true
}

// translate maps a virtual address to a physical one, raising a page fault
// on failure.
func (c *CPU) translate(va uint64, acc access) (uint64, bool) {
	paging, priv, tlb := c.dataPaging, c.dataPriv, &c.dtlb
	if acc == accFetch {
		paging, priv, tlb = c.fetchPaging, c.Priv, &c.itlb
	}
	if !paging {
		return va, true
	}
	vpn := va >> 12
	e := &tlb[vpn%tlbSize]
	if e.tag == vpn && c.permitted(e.flags, acc, priv) {
		return e.page | va&0xfff, true
	}
	page, flags, ok := c.walk(va, acc, priv)
	if !ok {
		c.exception(faultCause[acc], va)
		return 0, false
	}
	*e = tlbEntry{tag: vpn, page: page, flags: flags}
	return page | va&0xfff, true
}

// walk performs an Sv39 page table walk, updating A/D bits in hardware.
func (c *CPU) walk(va uint64, acc access, priv uint8) (page uint64, flags uint8, ok bool) {
	if uint64(int64(va<<25)>>25) != va {
		return 0, 0, false
	}
	table := (c.satp & (1<<44 - 1)) << 12
	for level := 2; level >= 0; level-- {
		addr := table + (va>>(12+9*uint(level))&0x1ff)*8
		off := addr - c.RAMBase
		if off >= uint64(len(c.RAM)) {
			return 0, 0, false
		}
		pte := binary.LittleEndian.Uint64(c.RAM[off:])
		if pte&pteV == 0 || (pte&pteR == 0 && pte&pteW != 0) || pte>>54 != 0 {
			return 0, 0, false
		}
		ppn := pte >> 10 & (1<<44 - 1)
		if pte&(pteR|pteX) == 0 {
			table = ppn << 12
			continue
		}
		// Leaf. Superpages must be aligned.
		mask := uint64(1)<<(9*uint(level)) - 1
		if ppn&mask != 0 {
			return 0, 0, false
		}
		f := uint8(pte)
		if !c.permitted(f|pteD, acc, priv) {
			return 0, 0, false
		}
		upd := pte | pteA
		if acc == accStore {
			upd |= pteD
		}
		if upd != pte {
			binary.LittleEndian.PutUint64(c.RAM[off:], upd)
		}
		return (ppn | (va>>12)&mask) << 12, uint8(upd), true
	}
	return 0, 0, false
}

// fetch returns the instruction at pc: 16 bits for compressed instructions,
// 32 bits otherwise.
func (c *CPU) fetch(pc uint64) (uint32, bool) {
	if pc>>12 == c.fetchTag {
		o := c.fetchOff + pc&0xfff
		if pc&0xfff <= 0xffc {
			return binary.LittleEndian.Uint32(c.RAM[o:]), true
		}
		lo := uint32(binary.LittleEndian.Uint16(c.RAM[o:]))
		if lo&3 != 3 {
			return lo, true
		}
	}
	lo, ok := c.fetch16(pc)
	if !ok || lo&3 != 3 {
		return lo, ok
	}
	hi, ok := c.fetch16(pc + 2)
	if !ok {
		return 0, false
	}
	return lo | hi<<16, true
}

func (c *CPU) fetch16(va uint64) (uint32, bool) {
	pa, ok := c.translate(va, accFetch)
	if !ok {
		return 0, false
	}
	off := pa - c.RAMBase
	if off >= uint64(len(c.RAM)) {
		c.exception(ExcInsnAccess, va)
		return 0, false
	}
	c.fetchTag = va >> 12
	c.fetchOff = off &^ 0xfff
	return uint32(binary.LittleEndian.Uint16(c.RAM[off:])), true
}

// load reads size bytes (1, 2, 4 or 8) at virtual address va. Misaligned
// accesses are supported, including ones that cross a page boundary.
func (c *CPU) load(va uint64, size int) (uint64, bool) {
	pa := va
	if c.dataPaging {
		// TLB hit fast path, inlined to avoid a call per access.
		e := &c.dtlb[(va>>12)%tlbSize]
		if e.tag != va>>12 || !c.permitted(e.flags, accLoad, c.dataPriv) {
			return c.loadSlow(va, size)
		}
		pa = e.page | va&0xfff
	}
	if off := pa - c.RAMBase; off <= uint64(len(c.RAM))-8 && va&0xfff+uint64(size) <= 0x1000 {
		r := c.RAM[off:]
		switch size {
		case 1:
			return uint64(r[0]), true
		case 2:
			return uint64(binary.LittleEndian.Uint16(r)), true
		case 4:
			return uint64(binary.LittleEndian.Uint32(r)), true
		default:
			return binary.LittleEndian.Uint64(r), true
		}
	}
	return c.loadSlow(va, size)
}

func (c *CPU) loadSlow(va uint64, size int) (uint64, bool) {
	if va&0xfff+uint64(size) > 0x1000 {
		return c.loadSplit(va, size)
	}
	pa, ok := c.translate(va, accLoad)
	if !ok {
		return 0, false
	}
	if off := pa - c.RAMBase; off < uint64(len(c.RAM)) {
		var b [8]byte
		copy(b[:size], c.RAM[off:])
		return binary.LittleEndian.Uint64(b[:]), true
	}
	v, ok := c.Bus.Read(pa, size)
	if !ok {
		c.exception(ExcLoadAccess, va)
	}
	return v, ok
}

func (c *CPU) loadSplit(va uint64, size int) (uint64, bool) {
	// Translate both pages first so a fault leaves no partial state.
	if _, ok := c.translate(va, accLoad); !ok {
		return 0, false
	}
	if _, ok := c.translate(va+uint64(size)-1, accLoad); !ok {
		return 0, false
	}
	var v uint64
	for i := 0; i < size; i++ {
		b, ok := c.load(va+uint64(i), 1)
		if !ok {
			return 0, false
		}
		v |= b << (8 * i)
	}
	return v, true
}

func (c *CPU) store(va uint64, size int, v uint64) bool {
	pa := va
	if c.dataPaging {
		e := &c.dtlb[(va>>12)%tlbSize]
		if e.tag != va>>12 || !c.permitted(e.flags, accStore, c.dataPriv) {
			return c.storeSlow(va, size, v)
		}
		pa = e.page | va&0xfff
	}
	if off := pa - c.RAMBase; off <= uint64(len(c.RAM))-8 && va&0xfff+uint64(size) <= 0x1000 {
		r := c.RAM[off:]
		switch size {
		case 1:
			r[0] = byte(v)
		case 2:
			binary.LittleEndian.PutUint16(r, uint16(v))
		case 4:
			binary.LittleEndian.PutUint32(r, uint32(v))
		default:
			binary.LittleEndian.PutUint64(r, v)
		}
		return true
	}
	return c.storeSlow(va, size, v)
}

func (c *CPU) storeSlow(va uint64, size int, v uint64) bool {
	if va&0xfff+uint64(size) > 0x1000 {
		return c.storeSplit(va, size, v)
	}
	pa, ok := c.translate(va, accStore)
	if !ok {
		return false
	}
	if off := pa - c.RAMBase; off < uint64(len(c.RAM)) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], v)
		copy(c.RAM[off:], b[:size])
		return true
	}
	if !c.Bus.Write(pa, size, v) {
		c.exception(ExcStoreAccess, va)
		return false
	}
	return true
}

func (c *CPU) storeSplit(va uint64, size int, v uint64) bool {
	if _, ok := c.translate(va, accStore); !ok {
		return false
	}
	if _, ok := c.translate(va+uint64(size)-1, accStore); !ok {
		return false
	}
	for i := 0; i < size; i++ {
		if !c.store(va+uint64(i), 1, v>>(8*i)) {
			return false
		}
	}
	return true
}

// ReadPhys copies guest physical memory, for firmware calls such as SBI DBCN.
func (c *CPU) ReadPhys(pa uint64, n int) ([]byte, bool) {
	off := pa - c.RAMBase
	if off >= uint64(len(c.RAM)) || off+uint64(n) > uint64(len(c.RAM)) {
		return nil, false
	}
	return c.RAM[off : off+uint64(n)], true
}

// FlushTLB discards all cached translations (for SBI remote fences).
func (c *CPU) FlushTLB() { c.flushTLB() }
