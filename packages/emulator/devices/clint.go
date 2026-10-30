package devices

// CLINT is the core-local interruptor for one hart: msip, mtimecmp and mtime.
// Under the Go SBI firmware Linux uses SBI timer calls instead, but M-mode
// software (and the DTB) still see a standard CLINT.
type CLINT struct {
	MSIP     bool
	MTimecmp uint64
	Time     func() uint64
	// Update is called after a register write so the owner can re-evaluate
	// MSIP/MTIP.
	Update func()
}

func NewCLINT(time func() uint64) *CLINT {
	return &CLINT{MTimecmp: ^uint64(0), Time: time}
}

func (c *CLINT) Read(off uint64, size int) (uint64, bool) {
	var v uint64
	base := off &^ 7
	switch base {
	case 0:
		if c.MSIP {
			v = 1
		}
	case 0x4000:
		v = c.MTimecmp
	case 0xbff8:
		v = c.Time()
	default:
		return 0, true
	}
	v >>= 8 * (off & 7)
	if size < 8 {
		v &= 1<<(8*size) - 1
	}
	return v, true
}

func (c *CLINT) Write(off uint64, size int, v uint64) bool {
	switch {
	case off == 0:
		c.MSIP = v&1 != 0
	case off&^7 == 0x4000:
		c.MTimecmp = merge(c.MTimecmp, off&7, size, v)
	}
	if c.Update != nil {
		c.Update()
	}
	return true
}

// merge writes size bytes of v at byte offset shift into old.
func merge(old, shift uint64, size int, v uint64) uint64 {
	if size == 8 {
		return v
	}
	mask := uint64(1)<<(8*size) - 1
	return old&^(mask<<(8*shift)) | (v&mask)<<(8*shift)
}
