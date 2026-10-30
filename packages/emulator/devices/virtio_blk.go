package devices

import "encoding/binary"

// VirtioBlock is a virtio block device backed by a Disk. Requests complete
// synchronously when the driver notifies.
type VirtioBlock struct {
	*Virtio
	Disk *Disk
}

const (
	blkTIn    = 0
	blkTOut   = 1
	blkTFlush = 4
	blkTGetID = 8
)

func NewVirtioBlock(d *Disk, ram []byte, ramBase uint64) *VirtioBlock {
	b := &VirtioBlock{Disk: d}
	b.Virtio = newVirtio(b, 1, ram, ramBase)
	return b
}

func (b *VirtioBlock) deviceID() uint32 { return 2 }
func (b *VirtioBlock) features() uint64 { return virtioF1 }

// config exposes the capacity in 512-byte sectors.
func (b *VirtioBlock) config(off uint64) byte {
	if off < 8 {
		return byte((b.Disk.Size() / 512) >> (8 * off))
	}
	return 0
}

func (b *VirtioBlock) notify(int) {
	done := false
	for {
		head, chain, ok := b.pop(0)
		if !ok {
			break
		}
		b.push(0, head, b.request(chain))
		done = true
	}
	if done {
		b.interrupt()
	}
}

// request executes one block request and returns the number of bytes
// written into device-writable buffers.
func (b *VirtioBlock) request(chain []descriptor) uint32 {
	if len(chain) < 2 {
		return 0
	}
	le := binary.LittleEndian
	hdr := b.mem(chain[0].addr, 16)
	statusByte := b.mem(chain[len(chain)-1].addr, 1)
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
			buf := b.mem(d.addr, uint64(d.len))
			if buf == nil || pos+uint64(d.len) > b.Disk.Size() {
				status = 1 // IOERR
				break
			}
			if typ == blkTIn {
				b.Disk.ReadAt(buf, pos)
				written += d.len
			} else {
				b.Disk.WriteAt(buf, pos)
			}
			pos += uint64(d.len)
		}
	case blkTFlush:
	case blkTGetID:
		if len(data) > 0 {
			if buf := b.mem(data[0].addr, uint64(data[0].len)); buf != nil {
				written += uint32(copy(buf, "bootlet-disk\x00"))
			}
		}
	default:
		status = 2 // UNSUPP
	}
	statusByte[0] = status
	return written + 1
}
