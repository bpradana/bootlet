package devices

// UART is a 16550A-compatible serial port. Transmission is instantaneous
// (the transmitter is always empty), received bytes are queued without limit.
type UART struct {
	ier, lcr, mcr, scr, dll, dlm, fcr uint8
	threPending                       bool
	rx                                []byte
	tx                                []byte

	// IRQ is called whenever the interrupt line may have changed.
	IRQ func(level bool)
}

const (
	lsrDR   = 0x01
	lsrTHRE = 0x20
	lsrTEMT = 0x40

	ierRDI  = 0x01
	ierTHRI = 0x02
)

func NewUART() *UART { return &UART{} }

// Input queues bytes for the guest to read.
func (u *UART) Input(b []byte) {
	u.rx = append(u.rx, b...)
	u.update()
}

// Output returns and clears the bytes written by the guest.
func (u *UART) Output() []byte {
	out := u.tx
	u.tx = nil
	return out
}

// TakeInput removes and returns up to n queued input bytes, for firmware
// console reads that bypass the UART registers.
func (u *UART) TakeInput(n int) []byte {
	n = min(n, len(u.rx))
	b := u.rx[:n:n]
	u.rx = u.rx[n:]
	u.update()
	return b
}

// HasOutput reports whether there is buffered output.
func (u *UART) HasOutput() bool { return len(u.tx) > 0 }

// Print adds bytes to the output stream (used by SBI console calls).
func (u *UART) Print(b []byte) { u.tx = append(u.tx, b...) }

func (u *UART) dlab() bool { return u.lcr&0x80 != 0 }

func (u *UART) Read(off uint64, _ int) (uint64, bool) {
	var v uint8
	switch off {
	case 0:
		if u.dlab() {
			v = u.dll
		} else if len(u.rx) > 0 {
			v = u.rx[0]
			u.rx = u.rx[1:]
			if len(u.rx) == 0 {
				u.rx = nil
			}
			u.update()
		}
	case 1:
		if u.dlab() {
			v = u.dlm
		} else {
			v = u.ier
		}
	case 2:
		v = u.iir()
		if v&0x0f == 0x02 {
			u.threPending = false // reading IIR clears a THRE interrupt
			u.update()
		}
	case 3:
		v = u.lcr
	case 4:
		v = u.mcr
	case 5:
		v = lsrTHRE | lsrTEMT
		if len(u.rx) > 0 {
			v |= lsrDR
		}
	case 6:
		v = 0xb0 // DCD, DSR, CTS
	case 7:
		v = u.scr
	}
	return uint64(v), true
}

func (u *UART) Write8(off uint64, v uint8) {
	switch off {
	case 0:
		if u.dlab() {
			u.dll = v
			return
		}
		u.tx = append(u.tx, v)
		u.threPending = true
		u.update()
	case 1:
		if u.dlab() {
			u.dlm = v
			return
		}
		if v&ierTHRI != 0 && u.ier&ierTHRI == 0 {
			u.threPending = true
		}
		u.ier = v & 0x0f
		u.update()
	case 2:
		u.fcr = v
		if v&0x02 != 0 { // clear receive FIFO
			u.rx = nil
			u.update()
		}
	case 3:
		u.lcr = v
	case 4:
		u.mcr = v
	case 7:
		u.scr = v
	}
}

func (u *UART) iir() uint8 {
	fifo := uint8(0)
	if u.fcr&1 != 0 {
		fifo = 0xc0
	}
	switch {
	case u.ier&ierRDI != 0 && len(u.rx) > 0:
		return fifo | 0x04
	case u.ier&ierTHRI != 0 && u.threPending:
		return fifo | 0x02
	}
	return fifo | 0x01
}

func (u *UART) update() {
	if u.IRQ != nil {
		u.IRQ(u.iir()&1 == 0)
	}
}
