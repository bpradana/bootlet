package devices

import "encoding/binary"

// VirtioConsole is a virtio console with a single named port that is not a
// console: the guest sees it as the character device /dev/vport0p0, a plain
// byte stream with no tty processing. Bytes queued with Deliver are written
// into the guest's receive buffers as they become available; bytes the guest
// writes are collected for Output.
type VirtioConsole struct {
	*Virtio
	// Name is announced to the guest (/sys/class/virtio-ports/vport0p0/name).
	Name string
	// Opened reports whether a guest process has the port open.
	Opened bool

	rx   []byte   // host → guest data waiting for receive buffers
	ctrl [][]byte // control messages waiting for control receive buffers
	out  []byte   // guest → host data not yet taken by Output
}

const (
	conQueueRX     = 0 // port 0 receiveq
	conQueueTX     = 1 // port 0 transmitq
	conQueueCtrlRX = 2 // control receiveq (device → driver)
	conQueueCtrlTX = 3 // control transmitq (driver → device)

	conFMultiport = 1 << 1

	// Control events (struct virtio_console_control).
	conDeviceReady = 0
	conDeviceAdd   = 1
	conPortReady   = 3
	conPortOpen    = 6
	conPortName    = 7
)

func NewVirtioConsole(name string, ram []byte, ramBase uint64) *VirtioConsole {
	c := &VirtioConsole{Name: name}
	c.Virtio = newVirtio(c, 4, ram, ramBase)
	return c
}

func (c *VirtioConsole) deviceID() uint32 { return 3 }
func (c *VirtioConsole) features() uint64 { return virtioF1 | conFMultiport }

// config: cols, rows (u16 each), then max_nr_ports (u32) = 1.
func (c *VirtioConsole) config(off uint64) byte {
	if off == 4 {
		return 1
	}
	return 0
}

// Reset drops queued data along with the transport state.
func (c *VirtioConsole) Reset() {
	c.rx, c.ctrl, c.out = nil, nil, nil
	c.Opened = false
	c.Virtio.Reset()
}

// Deliver queues bytes for the guest and fills any available buffers.
func (c *VirtioConsole) Deliver(b []byte) {
	c.rx = append(c.rx, b...)
	c.flush(conQueueRX)
}

// Output returns and clears the bytes the guest wrote since the last call.
func (c *VirtioConsole) Output() []byte {
	out := c.out
	c.out = nil
	return out
}

func (c *VirtioConsole) notify(q int) {
	switch q {
	case conQueueRX, conQueueCtrlRX: // the driver added receive buffers
		c.flush(q)
	case conQueueTX, conQueueCtrlTX:
		used := false
		for {
			head, chain, ok := c.pop(q)
			if !ok {
				break
			}
			var data []byte
			for _, d := range chain {
				if d.flags&descFWrite == 0 {
					data = append(data, c.mem(d.addr, uint64(d.len))...)
				}
			}
			c.push(q, head, 0)
			used = true
			if q == conQueueTX {
				c.out = append(c.out, data...)
			} else {
				c.control(data)
			}
		}
		if used {
			c.interrupt()
		}
	}
}

// control handles a message from the driver. The handshake: the driver
// reports DEVICE_READY, the device adds port 0, the driver reports
// PORT_READY, and the device names the port and marks the host side open
// (otherwise the guest's reads return EOF and its writes block).
func (c *VirtioConsole) control(msg []byte) {
	if len(msg) < 8 {
		return
	}
	le := binary.LittleEndian
	id, event, value := le.Uint32(msg), le.Uint16(msg[4:]), le.Uint16(msg[6:])
	switch event {
	case conDeviceReady:
		if value == 1 {
			c.sendControl(0, conDeviceAdd, 1, "")
		}
	case conPortReady:
		if id == 0 && value == 1 {
			c.sendControl(0, conPortName, 1, c.Name)
			c.sendControl(0, conPortOpen, 1, "")
		}
	case conPortOpen:
		if id == 0 {
			c.Opened = value == 1
		}
	}
}

func (c *VirtioConsole) sendControl(id uint32, event, value uint16, name string) {
	msg := make([]byte, 8+len(name))
	le := binary.LittleEndian
	le.PutUint32(msg, id)
	le.PutUint16(msg[4:], event)
	le.PutUint16(msg[6:], value)
	copy(msg[8:], name)
	c.ctrl = append(c.ctrl, msg)
	c.flush(conQueueCtrlRX)
}

// flush fills the driver's receive buffers on queue q: the data queue takes
// the byte stream in as many buffers as needed, the control queue takes one
// message per buffer.
func (c *VirtioConsole) flush(q int) {
	if c.status&4 == 0 { // DRIVER_OK not set yet
		return
	}
	used := false
	for (q == conQueueRX && len(c.rx) > 0) || (q == conQueueCtrlRX && len(c.ctrl) > 0) {
		head, chain, ok := c.pop(q)
		if !ok {
			break
		}
		src := c.rx
		if q == conQueueCtrlRX {
			src = c.ctrl[0]
			c.ctrl = c.ctrl[1:]
		}
		var written uint32
		for _, d := range chain {
			if d.flags&descFWrite == 0 || len(src) == 0 {
				continue
			}
			n := copy(c.mem(d.addr, uint64(d.len)), src)
			src = src[n:]
			written += uint32(n)
		}
		if q == conQueueRX {
			c.rx = src
		}
		c.push(q, head, written)
		used = true
	}
	if len(c.rx) == 0 {
		c.rx = nil
	}
	if used {
		c.interrupt()
	}
}
