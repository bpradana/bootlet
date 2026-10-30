package devices

import "encoding/binary"

// VirtioBlock is a virtio-mmio (version 2) block device with a single split
// virtqueue. Requests are completed synchronously when the driver notifies.
type VirtioBlock struct {
	Disk *Disk

	// RAM and RAMBase give the device DMA access to guest physical memory.
	RAM     []byte
	RAMBase uint64

	// IRQ is called when the interrupt line changes.
	IRQ func(level bool)

	devFeatSel, drvFeatSel uint32
	drvFeatures            uint64
	status                 uint32
	intStatus              uint32
	queueNum               uint32
	queueReady             bool
	desc, avail, used      uint64
	lastAvail              uint16
}

const (
	virtioQueueMax = 128
	virtioF1       = 1 << 32 // VIRTIO_F_VERSION_1

	descFNext  = 1
	descFWrite = 2

	blkTIn    = 0
	blkTOut   = 1
	blkTFlush = 4
	blkTGetID = 8
)

// Reset returns the device to its initial state (the disk is kept).
func (v *VirtioBlock) Reset() {
	*v = VirtioBlock{Disk: v.Disk, RAM: v.RAM, RAMBase: v.RAMBase, IRQ: v.IRQ}
	v.update()
}

func (v *VirtioBlock) Read(off uint64, _ int) (uint64, bool) {
	switch off {
	case 0x000:
		return 0x74726976, true // "virt"
	case 0x004:
		return 2, true
	case 0x008:
		return 2, true // block device
	case 0x00c:
		return 0x554d4551, true
	case 0x010:
		return (virtioF1 >> (32 * v.devFeatSel)) & 0xffffffff, true
	case 0x034:
		return virtioQueueMax, true
	case 0x044:
		if v.queueReady {
			return 1, true
		}
		return 0, true
	case 0x060:
		return uint64(v.intStatus), true
	case 0x070:
		return uint64(v.status), true
	case 0x0fc:
		return 0, true // config generation
	case 0x100: // capacity in 512-byte sectors (low, high)
		return (v.Disk.Size() / 512) & 0xffffffff, true
	case 0x104:
		return (v.Disk.Size() / 512) >> 32, true
	}
	return 0, true
}

func (v *VirtioBlock) Write(off uint64, _ int, val uint64) bool {
	x := uint32(val)
	switch off {
	case 0x014:
		v.devFeatSel = x & 1
	case 0x020:
		shift := 32 * uint64(v.drvFeatSel&1)
		v.drvFeatures = v.drvFeatures&^(0xffffffff<<shift) | uint64(x)<<shift
	case 0x024:
		v.drvFeatSel = x
	case 0x030: // queue select: only queue 0 exists
	case 0x038:
		v.queueNum = min(x, virtioQueueMax)
	case 0x044:
		v.queueReady = x&1 != 0
	case 0x050:
		v.process()
	case 0x064:
		v.intStatus &^= x
		v.update()
	case 0x070:
		if x == 0 {
			v.Reset()
			return true
		}
		v.status = x
	case 0x080:
		v.desc = v.desc&^0xffffffff | uint64(x)
	case 0x084:
		v.desc = v.desc&0xffffffff | uint64(x)<<32
	case 0x090:
		v.avail = v.avail&^0xffffffff | uint64(x)
	case 0x094:
		v.avail = v.avail&0xffffffff | uint64(x)<<32
	case 0x0a0:
		v.used = v.used&^0xffffffff | uint64(x)
	case 0x0a4:
		v.used = v.used&0xffffffff | uint64(x)<<32
	}
	return true
}

func (v *VirtioBlock) update() {
	if v.IRQ != nil {
		v.IRQ(v.intStatus != 0)
	}
}

// mem returns the guest memory at physical address pa, or nil.
func (v *VirtioBlock) mem(pa, n uint64) []byte {
	off := pa - v.RAMBase
	if off >= uint64(len(v.RAM)) || off+n > uint64(len(v.RAM)) {
		return nil
	}
	return v.RAM[off : off+n]
}

// process completes every request the driver has made available.
func (v *VirtioBlock) process() {
	if !v.queueReady || v.queueNum == 0 {
		return
	}
	le := binary.LittleEndian
	availIdx := v.mem(v.avail+2, 2)
	if availIdx == nil {
		return
	}
	for v.lastAvail != le.Uint16(availIdx) {
		slot := v.mem(v.avail+4+2*uint64(uint32(v.lastAvail)%v.queueNum), 2)
		if slot == nil {
			return
		}
		head := le.Uint16(slot)
		written := v.request(head)
		usedIdx := v.mem(v.used+2, 2)
		if usedIdx == nil {
			return
		}
		elem := v.mem(v.used+4+8*uint64(uint32(le.Uint16(usedIdx))%v.queueNum), 8)
		if elem == nil {
			return
		}
		le.PutUint32(elem, uint32(head))
		le.PutUint32(elem[4:], written)
		le.PutUint16(usedIdx, le.Uint16(usedIdx)+1)
		v.lastAvail++
	}
	v.intStatus |= 1
	v.update()
}

type descriptor struct {
	addr  uint64
	len   uint32
	flags uint16
}

// request executes the block request whose descriptor chain starts at head
// and returns the number of bytes written into device-writable buffers.
func (v *VirtioBlock) request(head uint16) uint32 {
	le := binary.LittleEndian
	var chain []descriptor
	for i, idx := 0, head; i < int(v.queueNum); i++ {
		d := v.mem(v.desc+16*uint64(uint32(idx)%v.queueNum), 16)
		if d == nil {
			return 0
		}
		chain = append(chain, descriptor{le.Uint64(d), le.Uint32(d[8:]), le.Uint16(d[12:])})
		if le.Uint16(d[12:])&descFNext == 0 {
			break
		}
		idx = le.Uint16(d[14:])
	}
	if len(chain) < 2 {
		return 0
	}
	hdr := v.mem(chain[0].addr, 16)
	last := chain[len(chain)-1]
	statusByte := v.mem(last.addr, 1)
	if hdr == nil || statusByte == nil {
		return 0
	}
	typ, sector := le.Uint32(hdr), le.Uint64(hdr[8:])
	data := chain[1 : len(chain)-1]
	pos := sector * 512
	var written uint32
	status := byte(0)
	switch typ {
	case blkTIn, blkTOut:
		for _, d := range data {
			buf := v.mem(d.addr, uint64(d.len))
			if buf == nil || pos+uint64(d.len) > v.Disk.Size() {
				status = 1 // IOERR
				break
			}
			if typ == blkTIn {
				v.Disk.ReadAt(buf, pos)
				written += d.len
			} else {
				v.Disk.WriteAt(buf, pos)
			}
			pos += uint64(d.len)
		}
	case blkTFlush:
	case blkTGetID:
		if len(data) > 0 {
			if buf := v.mem(data[0].addr, uint64(data[0].len)); buf != nil {
				written += uint32(copy(buf, "bootlet-disk\x00"))
			}
		}
	default:
		status = 2 // UNSUPP
	}
	statusByte[0] = status
	return written + 1
}
