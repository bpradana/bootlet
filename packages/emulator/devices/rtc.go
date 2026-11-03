package devices

import "time"

// RTC is a Goldfish real-time clock reporting the host's wall-clock time, so
// the guest boots with the correct date. Alarms are not supported.
type RTC struct {
	// Now, if set, replaces the host clock (for deterministic runs).
	Now  func() time.Time
	high uint32 // latched by reading TIME_LOW
}

func (r *RTC) Read(off uint64, _ int) (uint64, bool) {
	switch off {
	case 0x00: // TIME_LOW (latches TIME_HIGH)
		now := time.Now
		if r.Now != nil {
			now = r.Now
		}
		ns := uint64(now().UnixNano())
		r.high = uint32(ns >> 32)
		return ns & 0xffffffff, true
	case 0x04: // TIME_HIGH
		return uint64(r.high), true
	}
	return 0, true
}

func (r *RTC) Write(uint64, int, uint64) bool { return true }
