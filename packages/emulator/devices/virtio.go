package devices

import "encoding/binary"

// Virtio is a virtio-mmio (version 2) transport with split virtqueues. The
// device-specific behaviour (block, net) is supplied by a virtioDevice.
type Virtio struct {
	// RAM and RAMBase give the device DMA access to guest physical memory.
	RAM     []byte
	RAMBase uint64

	// IRQ is called when the interrupt line changes.
	IRQ func(level bool)

	dev                    virtioDevice
	devFeatSel, drvFeatSel uint32
	drvFeatures            uint64
	status                 uint32
	intStatus              uint32
	queueSel               uint32
	queues                 []virtqueue
}

type virtioDevice interface {
	deviceID() uint32
	features() uint64
	config(off uint64) byte
	// notify is called when the driver kicks queue q.
	notify(q int)
}

type virtqueue struct {
	num               uint32
	ready             bool
	desc, avail, used uint64
	lastAvail         uint16
}

type descriptor struct {
	addr  uint64
	len   uint32
	flags uint16
}

const (
	virtioQueueMax = 256
	virtioF1       = 1 << 32 // VIRTIO_F_VERSION_1

	descFNext  = 1
	descFWrite = 2
)

func newVirtio(dev virtioDevice, queues int, ram []byte, ramBase uint64) *Virtio {
	return &Virtio{dev: dev, queues: make([]virtqueue, queues), RAM: ram, RAMBase: ramBase}
}

// Reset returns the transport to its initial state.
func (v *Virtio) Reset() {
	v.devFeatSel, v.drvFeatSel, v.drvFeatures = 0, 0, 0
	v.status, v.intStatus, v.queueSel = 0, 0, 0
	clear(v.queues)
	v.update()
}

func (v *Virtio) queue() *virtqueue {
	if int(v.queueSel) < len(v.queues) {
		return &v.queues[v.queueSel]
	}
	return nil
}

func (v *Virtio) Read(off uint64, size int) (uint64, bool) {
	q := v.queue()
	switch off {
	case 0x000:
		return 0x74726976, true // "virt"
	case 0x004:
		return 2, true
	case 0x008:
		return uint64(v.dev.deviceID()), true
	case 0x00c:
		return 0x554d4551, true
	case 0x010:
		return (v.dev.features() >> (32 * v.devFeatSel)) & 0xffffffff, true
	case 0x034:
		if q == nil {
			return 0, true
		}
		return virtioQueueMax, true
	case 0x044:
		if q != nil && q.ready {
			return 1, true
		}
		return 0, true
	case 0x060:
		return uint64(v.intStatus), true
	case 0x070:
		return uint64(v.status), true
	case 0x0fc:
		return 0, true // config generation
	}
	if off >= 0x100 {
		var val uint64
		for i := 0; i < size; i++ {
			val |= uint64(v.dev.config(off-0x100+uint64(i))) << (8 * i)
		}
		return val, true
	}
	return 0, true
}

func (v *Virtio) Write(off uint64, _ int, val uint64) bool {
	x := uint32(val)
	q := v.queue()
	setLo := func(p *uint64) { *p = *p&^0xffffffff | uint64(x) }
	setHi := func(p *uint64) { *p = *p&0xffffffff | uint64(x)<<32 }
	switch off {
	case 0x014:
		v.devFeatSel = x & 1
	case 0x020:
		shift := 32 * uint64(v.drvFeatSel&1)
		v.drvFeatures = v.drvFeatures&^(0xffffffff<<shift) | uint64(x)<<shift
	case 0x024:
		v.drvFeatSel = x
	case 0x030:
		v.queueSel = x
	case 0x050:
		if int(x) < len(v.queues) {
			v.dev.notify(int(x))
		}
	case 0x064:
		v.intStatus &^= x
		v.update()
	case 0x070:
		if x == 0 {
			v.Reset()
			return true
		}
		v.status = x
	}
	if q == nil {
		return true
	}
	switch off {
	case 0x038:
		q.num = min(x, virtioQueueMax)
	case 0x044:
		q.ready = x&1 != 0
	case 0x080:
		setLo(&q.desc)
	case 0x084:
		setHi(&q.desc)
	case 0x090:
		setLo(&q.avail)
	case 0x094:
		setHi(&q.avail)
	case 0x0a0:
		setLo(&q.used)
	case 0x0a4:
		setHi(&q.used)
	}
	return true
}

func (v *Virtio) update() {
	if v.IRQ != nil {
		v.IRQ(v.intStatus != 0)
	}
}

// interrupt signals a used-buffer notification.
func (v *Virtio) interrupt() {
	v.intStatus |= 1
	v.update()
}

// mem returns the guest memory at physical address pa, or nil.
func (v *Virtio) mem(pa, n uint64) []byte {
	off := pa - v.RAMBase
	if off >= uint64(len(v.RAM)) || off+n > uint64(len(v.RAM)) {
		return nil
	}
	return v.RAM[off : off+n]
}

// pop takes the next available descriptor chain from queue qi.
func (v *Virtio) pop(qi int) (head uint16, chain []descriptor, ok bool) {
	q := &v.queues[qi]
	if !q.ready || q.num == 0 {
		return 0, nil, false
	}
	le := binary.LittleEndian
	availIdx := v.mem(q.avail+2, 2)
	if availIdx == nil || q.lastAvail == le.Uint16(availIdx) {
		return 0, nil, false
	}
	slot := v.mem(q.avail+4+2*uint64(uint32(q.lastAvail)%q.num), 2)
	if slot == nil {
		return 0, nil, false
	}
	q.lastAvail++
	head = le.Uint16(slot)
	for i, idx := 0, head; i < int(q.num); i++ {
		d := v.mem(q.desc+16*uint64(uint32(idx)%q.num), 16)
		if d == nil {
			break
		}
		chain = append(chain, descriptor{le.Uint64(d), le.Uint32(d[8:]), le.Uint16(d[12:])})
		if le.Uint16(d[12:])&descFNext == 0 {
			break
		}
		idx = le.Uint16(d[14:])
	}
	return head, chain, true
}

// push returns a descriptor chain to the driver with the number of bytes
// written into its device-writable buffers.
func (v *Virtio) push(qi int, head uint16, written uint32) {
	q := &v.queues[qi]
	le := binary.LittleEndian
	usedIdx := v.mem(q.used+2, 2)
	if usedIdx == nil {
		return
	}
	elem := v.mem(q.used+4+8*uint64(uint32(le.Uint16(usedIdx))%q.num), 8)
	if elem == nil {
		return
	}
	le.PutUint32(elem, uint32(head))
	le.PutUint32(elem[4:], written)
	le.PutUint16(usedIdx, le.Uint16(usedIdx)+1)
}
