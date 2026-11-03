package devices

import (
	"encoding/binary"
	"sort"
)

// VirtioGPU is a 2D virtio-gpu (device ID 16) with one scanout. It implements
// the 2D command set only: no 3D (virgl), EDID, blob resources or capsets.
// The guest's virtio-gpu driver keeps its pixels in its own memory (resource
// backing pages), copies changed regions into host-side resources with
// TRANSFER_TO_HOST_2D, and asks for them to be shown with RESOURCE_FLUSH. A
// flush copies the region of the scanout resource into the display's buffer,
// so the display (dirty tiles, splash) works as with a simple-framebuffer.
type VirtioGPU struct {
	*Virtio

	display   *Display
	resources map[uint32]*gpuResource
	used      int // bytes of host memory held by resources
	scanout   gpuScanout
	events    uint32 // events_read
	req, resp []byte // reused command buffers
}

type gpuResource struct {
	format        uint32
	width, height uint32
	data          []byte // width*height*4 bytes, in the guest's format
	backing       []gpuEntry
	ends          []uint64 // ends[i]: end of backing[i] in the backing's byte space
}

type gpuEntry struct {
	addr uint64
	len  uint32
}

// gpuScanout is the region (in resource pixels) of a resource on the display.
type gpuScanout struct {
	resource   uint32 // 0: disabled
	x, y, w, h uint32
}

const (
	gpuQueueControl = 0
	gpuQueueCursor  = 1

	gpuCmdGetDisplayInfo  = 0x0100
	gpuCmdResourceCreate  = 0x0101
	gpuCmdResourceUnref   = 0x0102
	gpuCmdSetScanout      = 0x0103
	gpuCmdResourceFlush   = 0x0104
	gpuCmdTransferToHost  = 0x0105
	gpuCmdAttachBacking   = 0x0106
	gpuCmdDetachBacking   = 0x0107
	gpuRespOKNoData       = 0x1100
	gpuRespOKDisplayInfo  = 0x1101
	gpuRespErrUnspec      = 0x1200
	gpuRespErrOutOfMemory = 0x1201
	gpuRespErrScanoutID   = 0x1202
	gpuRespErrResourceID  = 0x1203
	gpuRespErrParameter   = 0x1205

	gpuFlagFence  = 1
	gpuHeaderSize = 24
	gpuMaxScanout = 16 // entries in a display info response

	// gpuMaxMemory bounds the host memory the guest can make the device hold
	// in resources; gpuMaxRequest bounds one command (backing lists).
	gpuMaxMemory  = 64 << 20
	gpuMaxRequest = 1 << 20
	gpuMaxSide    = 8192
)

// Resource formats (virtio_gpu_formats), named by byte order in memory.
const (
	gpuFmtB8G8R8A8 = 1
	gpuFmtB8G8R8X8 = 2
	gpuFmtA8R8G8B8 = 3
	gpuFmtX8R8G8B8 = 4
	gpuFmtR8G8B8A8 = 67
	gpuFmtX8B8G8R8 = 68
	gpuFmtA8B8G8R8 = 121
	gpuFmtR8G8B8X8 = 134
)

// NewVirtioGPU returns a GPU that shows its scanout on d, whose buffer must
// be host memory (not guest RAM) in x8r8g8b8 format.
func NewVirtioGPU(d *Display, ram []byte, ramBase uint64) *VirtioGPU {
	g := &VirtioGPU{display: d, resources: map[uint32]*gpuResource{}}
	g.Virtio = newVirtio(g, 2, ram, ramBase)
	return g
}

func (g *VirtioGPU) deviceID() uint32 { return 16 }
func (g *VirtioGPU) features() uint64 { return virtioF1 }

// Reset drops every resource and disables the scanout; the display keeps
// what it shows until something else draws.
func (g *VirtioGPU) Reset() {
	clear(g.resources)
	g.used = 0
	g.scanout = gpuScanout{}
	g.events = 0
	g.Virtio.Reset()
}

// Config space: events_read, events_clear, num_scanouts, num_capsets (u32).
func (g *VirtioGPU) config(off uint64) byte {
	var v uint32
	switch off &^ 3 {
	case 0:
		v = g.events
	case 8:
		v = 1
	}
	return byte(v >> (8 * (off & 3)))
}

func (g *VirtioGPU) setConfig(off uint64, b byte) {
	if off>>2 == 1 { // events_clear
		g.events &^= uint32(b) << (8 * (off & 3))
	}
}

func (g *VirtioGPU) notify(q int) {
	used := false
	for {
		head, chain, ok := g.pop(q)
		if !ok {
			break
		}
		var written uint32
		if q == gpuQueueControl {
			written = g.command(chain)
		} // cursor commands (UPDATE_CURSOR, MOVE_CURSOR): no cursor is shown
		g.push(q, head, written)
		used = true
	}
	if used {
		g.interrupt()
	}
}

// command runs the request in a descriptor chain and writes its response;
// it returns the number of bytes written.
func (g *VirtioGPU) command(chain []descriptor) uint32 {
	g.req = g.req[:0]
	for _, d := range chain {
		if d.flags&descFWrite != 0 {
			continue
		}
		if len(g.req)+int(d.len) > gpuMaxRequest {
			return 0
		}
		if b := g.mem(d.addr, uint64(d.len)); b != nil {
			g.req = append(g.req, b...)
		}
	}
	if len(g.req) < gpuHeaderSize {
		return 0
	}
	le := binary.LittleEndian
	hdr := g.req[:gpuHeaderSize]
	body := g.req[gpuHeaderSize:]

	g.resp = append(g.resp[:0], make([]byte, gpuHeaderSize)...)
	typ := g.run(le.Uint32(hdr), body)
	le.PutUint32(g.resp, typ)
	if le.Uint32(hdr[4:])&gpuFlagFence != 0 {
		le.PutUint32(g.resp[4:], gpuFlagFence)
		copy(g.resp[8:16], hdr[8:16]) // fence_id
	}
	copy(g.resp[16:24], hdr[16:24]) // ctx_id, ring_idx

	var written uint32
	resp := g.resp
	for _, d := range chain {
		if d.flags&descFWrite == 0 || len(resp) == 0 {
			continue
		}
		n := min(uint64(d.len), uint64(len(resp)))
		if b := g.mem(d.addr, n); b != nil {
			copy(b, resp[:n])
			written += uint32(n)
		}
		resp = resp[n:]
	}
	return written
}

// run executes one command and returns the response type; data responses
// are appended to g.resp.
func (g *VirtioGPU) run(cmd uint32, b []byte) uint32 {
	le := binary.LittleEndian
	u32 := func(i int) uint32 { return le.Uint32(b[4*i:]) }
	need := func(n int) bool { return len(b) >= n }

	switch cmd {
	case gpuCmdGetDisplayInfo:
		for i := 0; i < gpuMaxScanout; i++ {
			var m [24]byte
			if i == 0 {
				le.PutUint32(m[8:], uint32(g.display.Width))
				le.PutUint32(m[12:], uint32(g.display.Height))
				le.PutUint32(m[16:], 1) // enabled
			}
			g.resp = append(g.resp, m[:]...)
		}
		return gpuRespOKDisplayInfo

	case gpuCmdResourceCreate: // resource_id, format, width, height
		if !need(16) {
			return gpuRespErrParameter
		}
		id, format, w, h := u32(0), u32(1), u32(2), u32(3)
		if id == 0 || g.resources[id] != nil {
			return gpuRespErrResourceID
		}
		if _, ok := gpuChannels(format); !ok || w == 0 || h == 0 || w > gpuMaxSide || h > gpuMaxSide {
			return gpuRespErrParameter
		}
		size := int(w) * int(h) * 4
		if g.used+size > gpuMaxMemory {
			return gpuRespErrOutOfMemory
		}
		g.used += size
		g.resources[id] = &gpuResource{format: format, width: w, height: h, data: make([]byte, size)}
		return gpuRespOKNoData

	case gpuCmdResourceUnref: // resource_id
		if !need(4) {
			return gpuRespErrParameter
		}
		r := g.resources[u32(0)]
		if r == nil {
			return gpuRespErrResourceID
		}
		if g.scanout.resource == u32(0) {
			g.disable()
		}
		g.used -= len(r.data)
		delete(g.resources, u32(0))
		return gpuRespOKNoData

	case gpuCmdAttachBacking: // resource_id, nr_entries, then {addr u64, length u32, padding u32}
		if !need(8) {
			return gpuRespErrParameter
		}
		r := g.resources[u32(0)]
		if r == nil {
			return gpuRespErrResourceID
		}
		n := int(u32(1))
		if r.backing != nil || n == 0 || len(b) < 8+16*n {
			return gpuRespErrParameter
		}
		r.backing = make([]gpuEntry, n)
		r.ends = make([]uint64, n)
		var end uint64
		for i := range r.backing {
			e := b[8+16*i:]
			r.backing[i] = gpuEntry{le.Uint64(e), le.Uint32(e[8:])}
			end += uint64(r.backing[i].len)
			r.ends[i] = end
		}
		return gpuRespOKNoData

	case gpuCmdDetachBacking: // resource_id
		if !need(4) {
			return gpuRespErrParameter
		}
		r := g.resources[u32(0)]
		if r == nil {
			return gpuRespErrResourceID
		}
		r.backing, r.ends = nil, nil
		return gpuRespOKNoData

	case gpuCmdSetScanout: // rect, scanout_id, resource_id
		if !need(24) {
			return gpuRespErrParameter
		}
		if u32(4) != 0 {
			return gpuRespErrScanoutID
		}
		id := u32(5)
		if id == 0 {
			g.disable()
			return gpuRespOKNoData
		}
		r := g.resources[id]
		if r == nil {
			return gpuRespErrResourceID
		}
		s := gpuScanout{id, u32(0), u32(1), u32(2), u32(3)}
		if !r.contains(s.x, s.y, s.w, s.h) {
			return gpuRespErrParameter
		}
		g.scanout = s
		g.show(r, s.x, s.y, s.w, s.h)
		return gpuRespOKNoData

	case gpuCmdTransferToHost: // rect, offset u64, resource_id, padding
		if !need(32) {
			return gpuRespErrParameter
		}
		r := g.resources[u32(6)]
		if r == nil {
			return gpuRespErrResourceID
		}
		x, y, w, h := u32(0), u32(1), u32(2), u32(3)
		if r.backing == nil || !r.contains(x, y, w, h) {
			return gpuRespErrParameter
		}
		r.transfer(g.RAM, g.RAMBase, le.Uint64(b[16:]), x, y, w, h)
		return gpuRespOKNoData

	case gpuCmdResourceFlush: // rect, resource_id, padding
		if !need(20) {
			return gpuRespErrParameter
		}
		id := u32(4)
		r := g.resources[id]
		if r == nil {
			return gpuRespErrResourceID
		}
		if g.scanout.resource == id {
			g.show(r, u32(0), u32(1), u32(2), u32(3))
		}
		return gpuRespOKNoData
	}
	return gpuRespErrUnspec
}

func (r *gpuResource) contains(x, y, w, h uint32) bool {
	return uint64(x)+uint64(w) <= uint64(r.width) && uint64(y)+uint64(h) <= uint64(r.height)
}

// transfer copies rows of a rectangle from the guest's backing pages into
// the resource. Row i of the rectangle starts at byte off + i*stride of the
// backing, as in QEMU.
func (r *gpuResource) transfer(ram []byte, ramBase, off uint64, x, y, w, h uint32) {
	stride := uint64(r.width) * 4
	n := uint64(w) * 4
	for i := uint64(0); i < uint64(h); i++ {
		dst := r.data[(uint64(y)+i)*stride+uint64(x)*4:][:n]
		r.read(ram, ramBase, off+i*stride, dst)
	}
}

// read fills dst from the backing, starting at byte off; bytes outside the
// backing or guest RAM are left as they were.
func (r *gpuResource) read(ram []byte, ramBase, off uint64, dst []byte) {
	i := sort.Search(len(r.ends), func(i int) bool { return r.ends[i] > off })
	for ; i < len(r.backing) && len(dst) > 0; i++ {
		e := r.backing[i]
		start := r.ends[i] - uint64(e.len)
		skip := off - start
		n := min(uint64(e.len)-skip, uint64(len(dst)))
		if pa := e.addr + skip - ramBase; e.addr+skip >= ramBase && pa+n <= uint64(len(ram)) {
			copy(dst, ram[pa:pa+n])
		}
		dst = dst[n:]
		off += n
	}
}

// show copies the part of rectangle (x, y, w, h) of resource r that is on
// the scanout to the display.
func (g *VirtioGPU) show(r *gpuResource, x, y, w, h uint32) {
	s := g.scanout
	d := g.display
	x0, y0 := max(x, s.x), max(y, s.y)
	x1 := min(x+w, s.x+min(s.w, uint32(d.Width)))
	y1 := min(y+h, s.y+min(s.h, uint32(d.Height)))
	if x0 >= x1 || y0 >= y1 {
		return
	}
	ch, _ := gpuChannels(r.format)
	stride := int(r.width) * 4
	for row := y0; row < y1; row++ {
		src := r.data[int(row)*stride+int(x0)*4 : int(row)*stride+int(x1)*4]
		o := int(row-s.y)*d.Stride + int(x0-s.x)*4
		dst := d.fb[o : o+len(src)]
		if ch == [3]int{2, 1, 0} {
			copy(dst, src)
			continue
		}
		for i := 0; i < len(src); i += 4 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+ch[2]], src[i+ch[1]], src[i+ch[0]], 0
		}
	}
	d.Damage()
}

// disable turns the scanout off: the display goes black.
func (g *VirtioGPU) disable() {
	g.scanout = gpuScanout{}
	clear(g.display.fb)
	g.display.Damage()
}

// gpuChannels returns the byte offsets of red, green and blue in a pixel of
// the format.
func gpuChannels(format uint32) ([3]int, bool) {
	switch format {
	case gpuFmtB8G8R8A8, gpuFmtB8G8R8X8:
		return [3]int{2, 1, 0}, true
	case gpuFmtA8R8G8B8, gpuFmtX8R8G8B8:
		return [3]int{1, 2, 3}, true
	case gpuFmtR8G8B8A8, gpuFmtR8G8B8X8:
		return [3]int{0, 1, 2}, true
	case gpuFmtA8B8G8R8, gpuFmtX8B8G8R8:
		return [3]int{3, 2, 1}, true
	}
	return [3]int{}, false
}
