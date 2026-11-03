package devices

import "encoding/binary"

// VirtioInput is a virtio keyboard (virtio-input, device ID 18). Key presses
// and releases from the page arrive as Linux input events, so the guest sees
// a real keyboard with key-up events (/dev/input/eventN, and the console).
type VirtioInput struct {
	*Virtio
	Name string

	select_, subsel byte
	pending         []byte // 8-byte events waiting for event buffers
}

const (
	inQueueEvent  = 0
	inQueueStatus = 1

	inCfgIDName  = 0x01
	inCfgIDDevid = 0x03
	inCfgEvBits  = 0x11

	evSyn = 0x00
	evKey = 0x01

	// maxPendingInput bounds events held while the guest posts no buffers
	// (e.g. before the driver loads): about 170 key strokes.
	maxPendingInput = 8 * 1024
)

func NewVirtioInput(name string, ram []byte, ramBase uint64) *VirtioInput {
	in := &VirtioInput{Name: name}
	in.Virtio = newVirtio(in, 2, ram, ramBase)
	return in
}

func (in *VirtioInput) deviceID() uint32 { return 18 }
func (in *VirtioInput) features() uint64 { return virtioF1 }

// Reset drops queued events along with the transport state.
func (in *VirtioInput) Reset() {
	in.pending = nil
	in.select_, in.subsel = 0, 0
	in.Virtio.Reset()
}

// config space: select (u8), subsel (u8), size (u8), reserved[5], data[128].
func (in *VirtioInput) config(off uint64) byte {
	data := in.configData()
	switch {
	case off == 0:
		return in.select_
	case off == 1:
		return in.subsel
	case off == 2:
		return byte(len(data))
	case off >= 8 && off-8 < uint64(len(data)):
		return data[off-8]
	}
	return 0
}

func (in *VirtioInput) setConfig(off uint64, b byte) {
	switch off {
	case 0:
		in.select_ = b
	case 1:
		in.subsel = b
	}
}

func (in *VirtioInput) configData() []byte {
	switch in.select_ {
	case inCfgIDName:
		if in.subsel == 0 {
			return []byte(in.Name)
		}
	case inCfgIDDevid:
		if in.subsel == 0 {
			d := make([]byte, 8)
			le := binary.LittleEndian
			le.PutUint16(d, 0x06) // BUS_VIRTUAL
			le.PutUint16(d[2:], 0x1af4)
			le.PutUint16(d[4:], 0x0001)
			le.PutUint16(d[6:], 0x0001)
			return d
		}
	case inCfgEvBits:
		if in.subsel == evKey {
			// Every key code below 256 (KEY_ESC .. KEY_MICMUTE and friends).
			bits := make([]byte, 32)
			for i := range bits {
				bits[i] = 0xff
			}
			bits[0] &^= 1 // KEY_RESERVED
			return bits
		}
	}
	return nil
}

// Key queues a key press (down) or release followed by a sync event.
func (in *VirtioInput) Key(code uint16, down bool) {
	var v uint32
	if down {
		v = 1
	}
	if len(in.pending)+16 > maxPendingInput {
		return // the guest isn't reading: drop rather than grow without bound
	}
	in.pending = appendEvent(in.pending, evKey, code, v)
	in.pending = appendEvent(in.pending, evSyn, 0, 0)
	in.flush()
}

func appendEvent(b []byte, typ, code uint16, value uint32) []byte {
	b = binary.LittleEndian.AppendUint16(b, typ)
	b = binary.LittleEndian.AppendUint16(b, code)
	return binary.LittleEndian.AppendUint32(b, value)
}

func (in *VirtioInput) notify(q int) {
	switch q {
	case inQueueEvent:
		in.flush()
	case inQueueStatus: // LED state from the guest: accept and ignore
		used := false
		for {
			head, _, ok := in.pop(inQueueStatus)
			if !ok {
				break
			}
			in.push(inQueueStatus, head, 0)
			used = true
		}
		if used {
			in.interrupt()
		}
	}
}

// flush moves pending events into the guest's event buffers, one per buffer.
func (in *VirtioInput) flush() {
	used := false
	for len(in.pending) >= 8 {
		head, chain, ok := in.pop(inQueueEvent)
		if !ok {
			break
		}
		var written uint32
		if len(chain) > 0 && chain[0].flags&descFWrite != 0 && chain[0].len >= 8 {
			if dst := in.mem(chain[0].addr, 8); dst != nil {
				copy(dst, in.pending[:8])
				written = 8
			}
		}
		in.pending = in.pending[8:]
		in.push(inQueueEvent, head, written)
		used = true
	}
	if len(in.pending) == 0 {
		in.pending = nil
	}
	if used {
		in.interrupt()
	}
}
