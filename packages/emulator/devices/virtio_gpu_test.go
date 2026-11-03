package devices

import (
	"encoding/binary"
	"testing"
)

// gpuDriver sends control commands to a VirtioGPU the way Linux does: a
// readable header+command descriptor, optionally a second readable one
// (backing entries), and a writable response descriptor.
type gpuDriver struct {
	*testDriver
	slot uint16
}

const (
	gpuTestReq  = testRAMBase + 0x4000 // request bytes
	gpuTestReq2 = testRAMBase + 0x5000
	gpuTestResp = testRAMBase + 0x6000
)

func (d *gpuDriver) cmd(t *testing.T, typ, flags uint32, fence uint64, args []byte, extra []byte) []byte {
	t.Helper()
	le := binary.LittleEndian
	req := make([]byte, gpuHeaderSize, gpuHeaderSize+len(args))
	le.PutUint32(req, typ)
	le.PutUint32(req[4:], flags)
	le.PutUint64(req[8:], fence)
	req = append(req, args...)
	copy(d.ram[gpuTestReq-testRAMBase:], req)
	copy(d.ram[gpuTestReq2-testRAMBase:], extra)
	clear(d.ram[gpuTestResp-testRAMBase : gpuTestResp-testRAMBase+0x1000])

	type desc struct {
		addr  uint64
		n     uint32
		flags uint16
	}
	chain := []desc{{gpuTestReq, uint32(len(req)), 0}}
	if extra != nil {
		chain = append(chain, desc{gpuTestReq2, uint32(len(extra)), 0})
	}
	chain = append(chain, desc{gpuTestResp, 0x1000, descFWrite})
	head := d.slot
	for i, c := range chain {
		idx := (head + uint16(i)) % 16
		e := d.ram[16*int(idx):]
		le.PutUint64(e, c.addr)
		le.PutUint32(e[8:], c.n)
		f := c.flags
		if i+1 < len(chain) {
			f |= descFNext
			le.PutUint16(e[14:], (idx+1)%16)
		}
		le.PutUint16(e[12:], f)
	}
	d.slot = (d.slot + uint16(len(chain))) % 16
	i := d.next[0]
	le.PutUint16(d.ram[0x100+4+2*int(i%16):], head)
	d.next[0]++
	le.PutUint16(d.ram[0x100+2:], d.next[0])
	d.v.Write(0x050, 4, 0)
	if lens := d.used(0); len(lens) != 1 {
		t.Fatalf("command %#x: used %v", typ, lens)
	}
	return d.ram[gpuTestResp-testRAMBase:]
}

func u32s(v ...uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.LittleEndian.AppendUint32(b, x)
	}
	return b
}

func TestVirtioGPU(t *testing.T) {
	le := binary.LittleEndian
	const w, h = 64, 32
	ram := make([]byte, 1<<20)
	disp := NewHostDisplay(w, h)
	g := NewVirtioGPU(disp, ram, testRAMBase)
	irq := false
	g.IRQ = func(level bool) { irq = level }
	d := &gpuDriver{testDriver: newTestDriver(g.Virtio, ram, 2)}
	disp.Flush() // the initial full frame
	ok := func(resp []byte, want uint32) {
		t.Helper()
		if got := le.Uint32(resp); got != want {
			t.Fatalf("response %#x, want %#x", got, want)
		}
	}

	if n, _ := g.Read(0x108, 4); n != 1 {
		t.Fatalf("num_scanouts = %d", n)
	}
	r := d.cmd(t, gpuCmdGetDisplayInfo, 0, 0, nil, nil)
	ok(r, gpuRespOKDisplayInfo)
	if le.Uint32(r[24+8:]) != w || le.Uint32(r[24+12:]) != h || le.Uint32(r[24+16:]) != 1 || le.Uint32(r[48+16:]) != 0 {
		t.Fatalf("display info % x", r[24:72])
	}
	if !irq {
		t.Fatal("no interrupt for a used buffer")
	}

	ok(d.cmd(t, gpuCmdResourceCreate, 0, 0, u32s(1, gpuFmtB8G8R8X8, w, h), nil), gpuRespOKNoData)
	ok(d.cmd(t, gpuCmdResourceCreate, 0, 0, u32s(1, gpuFmtB8G8R8X8, w, h), nil), gpuRespErrResourceID)
	ok(d.cmd(t, gpuCmdResourceCreate, 0, 0, u32s(2, 999, w, h), nil), gpuRespErrParameter)

	// The guest's pixels are split over two backing entries; the split
	// falls inside a row.
	const back1, back2 = testRAMBase + 0x10000, testRAMBase + 0x40000
	const split = 3*w*4 + 40
	entries := make([]byte, 32)
	le.PutUint64(entries, back1)
	le.PutUint32(entries[8:], split)
	le.PutUint64(entries[16:], back2)
	le.PutUint32(entries[24:], w*h*4-split)
	ok(d.cmd(t, gpuCmdAttachBacking, 0, 0, u32s(1, 2), entries), gpuRespOKNoData)
	guestPx := func(x, y int) []byte {
		o := (y*w + x) * 4
		if o < split {
			return ram[back1-testRAMBase+o:]
		}
		return ram[back2-testRAMBase+o-split:]
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			le.PutUint32(guestPx(x, y), uint32(y<<16|x<<8|0x11))
		}
	}

	// Transfer and flush a rectangle crossing the split; nothing shows
	// before the scanout is set.
	ok(d.cmd(t, gpuCmdTransferToHost, 0, 0, append(u32s(8, 2, 16, 4), append(binary.LittleEndian.AppendUint64(nil, (2*w+8)*4), u32s(1, 0)...)...), nil), gpuRespOKNoData)
	ok(d.cmd(t, gpuCmdResourceFlush, 0, 0, u32s(8, 2, 16, 4, 1, 0), nil), gpuRespOKNoData)
	if rects, _ := disp.Flush(); rects != nil {
		t.Fatalf("flush without a scanout showed %+v", rects)
	}
	ok(d.cmd(t, gpuCmdSetScanout, 0, 0, u32s(0, 0, w, h, 0, 2), nil), gpuRespErrResourceID)
	ok(d.cmd(t, gpuCmdSetScanout, 0, 0, u32s(0, 0, w, h, 0, 1), nil), gpuRespOKNoData)
	rects, data := disp.Flush()
	if len(rects) == 0 {
		t.Fatal("scanout not shown")
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var want uint32
			if x >= 8 && x < 24 && y >= 2 && y < 6 {
				want = uint32(y<<16 | x<<8 | 0x11)
			}
			if got := le.Uint32(disp.fb[y*w*4+x*4:]) &^ 0xff000000; got != want {
				t.Fatalf("pixel (%d,%d) = %#x, want %#x", x, y, got, want)
			}
		}
	}
	_ = data

	// A fenced flush echoes the fence, and only the flushed rectangle changes.
	le.PutUint32(guestPx(40, 20), 0x00abcdef)
	ok(d.cmd(t, gpuCmdTransferToHost, 0, 0, append(u32s(0, 0, w, h), append(binary.LittleEndian.AppendUint64(nil, 0), u32s(1, 0)...)...), nil), gpuRespOKNoData)
	if rects, _ := disp.Flush(); rects != nil {
		t.Fatalf("transfer alone changed the display: %+v", rects)
	}
	r = d.cmd(t, gpuCmdResourceFlush, gpuFlagFence, 77, u32s(32, 16, 32, 16, 1, 0), nil)
	ok(r, gpuRespOKNoData)
	if le.Uint32(r[4:]) != gpuFlagFence || le.Uint64(r[8:]) != 77 {
		t.Fatalf("fence not echoed: % x", r[:24])
	}
	rects, data = disp.Flush()
	if len(rects) != 1 || rects[0] != (Rect{0, 16, 64, 16, 0}) {
		t.Fatalf("rects %+v", rects)
	}
	if o := ((20-16)*64 + 40) * 4; data[o] != 0xab || data[o+1] != 0xcd || data[o+2] != 0xef || data[o+3] != 0xff {
		t.Fatalf("pixel % x", data[o:o+4])
	}

	// Unknown commands fail; unref of the scanout resource blanks the display.
	ok(d.cmd(t, 0x0200, 0, 0, nil, nil), gpuRespErrUnspec)
	ok(d.cmd(t, gpuCmdResourceUnref, 0, 0, u32s(1), nil), gpuRespOKNoData)
	if g.used != 0 || g.scanout.resource != 0 {
		t.Fatalf("after unref: used %d, scanout %+v", g.used, g.scanout)
	}
	for _, b := range disp.fb {
		if b != 0 {
			t.Fatal("display not blank after its resource was released")
		}
	}

	// Host memory is bounded.
	ok(d.cmd(t, gpuCmdResourceCreate, 0, 0, u32s(3, gpuFmtB8G8R8X8, 8192, 8192), nil), gpuRespErrOutOfMemory)
}

func TestVirtioGPUFormats(t *testing.T) {
	disp := NewHostDisplay(1, 1)
	g := NewVirtioGPU(disp, nil, testRAMBase)
	for format, px := range map[uint32][4]byte{
		gpuFmtB8G8R8X8: {0x56, 0x34, 0x12, 0},
		gpuFmtX8R8G8B8: {0, 0x12, 0x34, 0x56},
		gpuFmtR8G8B8A8: {0x12, 0x34, 0x56, 0xff},
		gpuFmtA8B8G8R8: {0xff, 0x56, 0x34, 0x12},
	} {
		r := &gpuResource{format: format, width: 1, height: 1, data: px[:]}
		g.scanout = gpuScanout{1, 0, 0, 1, 1}
		g.show(r, 0, 0, 1, 1)
		if got := binary.LittleEndian.Uint32(disp.fb); got != 0x123456 {
			t.Errorf("format %d: %#x", format, got)
		}
	}
}
