package machine

import (
	"bytes"

	"bootlet/emulator/cpu"
)

// SBI (RISC-V Supervisor Binary Interface) firmware implemented in Go. The
// kernel runs in S-mode and its ecalls land here instead of in an M-mode
// firmware such as OpenSBI.

const (
	sbiSuccess        = 0
	sbiErrNotSupp     = -2
	sbiErrInvalid     = -3
	sbiErrAlreadyAvai = -6

	extBase   = 0x10
	extTime   = 0x54494d45
	extIPI    = 0x735049
	extRFence = 0x52464e43
	extHSM    = 0x48534d
	extSRST   = 0x53525354
	extDBCN   = 0x4442434e
)

func (m *Machine) sbi(c *cpu.CPU) {
	eid, fid := c.X[17], c.X[16]
	a0, a1, a2 := c.X[10], c.X[11], c.X[12]

	if eid <= 8 { // legacy extensions return a single value in a0
		c.X[10] = uint64(m.legacy(c, eid, a0))
		return
	}
	var err int64 = sbiSuccess
	var val uint64
	switch eid {
	case extBase:
		switch fid {
		case 0: // spec version 2.0
			val = 2 << 24
		case 1: // implementation id
			val = 0xb007
		case 2:
			val = 1
		case 3: // probe extension
			switch a0 {
			case extBase, extTime, extIPI, extRFence, extHSM, extSRST, extDBCN, 0, 1, 2, 3, 4, 5, 6, 7, 8:
				val = 1
			}
		case 4, 5, 6: // mvendorid, marchid, mimpid
		default:
			err = sbiErrNotSupp
		}
	case extTime:
		if fid != 0 {
			err = sbiErrNotSupp
			break
		}
		m.setTimer(a0)
	case extIPI:
		if fid != 0 {
			err = sbiErrNotSupp
			break
		}
		m.sendIPI(a0, a1)
	case extRFence:
		c.FlushTLB()
	case extHSM:
		switch fid {
		case 0: // hart_start
			err = sbiErrInvalid
			if a0 == 0 {
				err = sbiErrAlreadyAvai
			}
		case 2: // hart_get_status
			if a0 != 0 {
				err = sbiErrInvalid
			}
		default:
			err = sbiErrNotSupp
		}
	case extSRST:
		if fid != 0 {
			err = sbiErrNotSupp
			break
		}
		if a0 == 0 {
			m.halted = true
		} else {
			m.reboot = true
		}
	case extDBCN:
		switch fid {
		case 0: // write(num_bytes, base_lo, base_hi)
			b, ok := c.ReadPhys(a1|a2<<32, int(a0))
			if !ok {
				err = sbiErrInvalid
				break
			}
			m.consolePrint(b)
			val = a0
		case 1: // read
			b, ok := c.ReadPhys(a1|a2<<32, int(a0))
			if !ok {
				err = sbiErrInvalid
				break
			}
			val = uint64(copy(b, m.UART.TakeInput(len(b))))
		case 2: // write_byte
			m.consolePrint([]byte{byte(a0)})
		default:
			err = sbiErrNotSupp
		}
	default:
		err = sbiErrNotSupp
	}
	c.X[10] = uint64(err)
	c.X[11] = val
}

func (m *Machine) legacy(c *cpu.CPU, eid, a0 uint64) int64 {
	switch eid {
	case 0:
		m.setTimer(a0)
	case 1:
		m.consolePrint([]byte{byte(a0)})
	case 2:
		if b := m.UART.TakeInput(1); len(b) == 1 {
			return int64(b[0])
		}
		return -1
	case 3:
		c.SetPending(cpu.IntSSI, false)
	case 4:
		m.CPU.SetPending(cpu.IntSSI, true)
	case 5, 6, 7:
		c.FlushTLB()
	case 8:
		m.halted = true
	}
	return 0
}

// consolePrint writes SBI console output. Like OpenSBI, it turns "\n" into
// "\r\n": the kernel's SBI early console sends bare newlines, unlike the
// 8250 driver which does the conversion itself.
func (m *Machine) consolePrint(b []byte) {
	if !bytes.Contains(b, []byte{'\n'}) {
		m.UART.Print(b)
		return
	}
	m.UART.Print(bytes.ReplaceAll(b, []byte{'\n'}, []byte{'\r', '\n'}))
}

func (m *Machine) setTimer(t uint64) {
	m.sbiTimer = t
	m.CPU.SetPending(cpu.IntSTI, m.now() >= t)
}

// sendIPI raises a supervisor software interrupt if hart 0 is targeted.
func (m *Machine) sendIPI(mask, base uint64) {
	if base == ^uint64(0) || (base == 0 && mask&1 != 0) {
		m.CPU.SetPending(cpu.IntSSI, true)
	}
}
