// Package devices implements the memory-mapped peripherals of the virtual
// machine and the bus that routes physical addresses to them.
package devices

// Device is a memory-mapped peripheral. Offsets are relative to its base.
type Device interface {
	Read(off uint64, size int) (uint64, bool)
	Write(off uint64, size int, v uint64) bool
}

type mapping struct {
	base, size uint64
	dev        Device
}

// Bus routes physical MMIO accesses to devices. It implements cpu.Bus.
type Bus struct {
	maps []mapping
}

func (b *Bus) Map(base, size uint64, d Device) {
	b.maps = append(b.maps, mapping{base, size, d})
}

func (b *Bus) find(addr uint64) (Device, uint64) {
	for i := range b.maps {
		m := &b.maps[i]
		if addr-m.base < m.size {
			return m.dev, addr - m.base
		}
	}
	return nil, 0
}

func (b *Bus) Read(addr uint64, size int) (uint64, bool) {
	d, off := b.find(addr)
	if d == nil {
		return 0, false
	}
	return d.Read(off, size)
}

func (b *Bus) Write(addr uint64, size int, v uint64) bool {
	d, off := b.find(addr)
	if d == nil {
		return false
	}
	return d.Write(off, size, v)
}

// Write implements Device for the UART (byte-wide registers).
func (u *UART) Write(off uint64, _ int, v uint64) bool {
	u.Write8(off, uint8(v))
	return true
}
