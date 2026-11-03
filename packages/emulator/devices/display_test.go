package devices

import (
	"encoding/binary"
	"testing"
)

func TestDisplayFlush(t *testing.T) {
	const w, h, stride = 640, 480, 640 * 4
	fb := make([]byte, stride*h)
	d := NewDisplay(fb, w, h, stride)

	// The first flush sends everything as one rectangle.
	rects, data := d.Flush()
	if len(rects) != 1 || rects[0] != (Rect{0, 0, w, h, 0}) || len(data) != w*h*4 {
		t.Fatalf("first flush: %+v (%d bytes)", rects, len(data))
	}
	if rects, _ := d.Flush(); rects != nil {
		t.Fatalf("unchanged screen flushed %+v", rects)
	}

	px := func(x, y int, v uint32) { binary.LittleEndian.PutUint32(fb[y*stride+x*4:], v) }
	px(70, 20, 0x00123456) // x8r8g8b8: R=0x12 G=0x34 B=0x56
	rects, data = d.Flush()
	if len(rects) != 1 || rects[0] != (Rect{64, 16, 64, 16, 0}) {
		t.Fatalf("one pixel: %+v", rects)
	}
	o := ((20-16)*64 + (70 - 64)) * 4
	if got := data[o : o+4]; got[0] != 0x12 || got[1] != 0x34 || got[2] != 0x56 || got[3] != 0xff {
		t.Fatalf("pixel converted to %x", got)
	}

	// A vertical stripe spanning two tile columns and three tile rows
	// becomes a single rectangle; a separate pixel far away its own.
	for y := 100; y < 140; y++ {
		px(120, y, 1)
		px(130, y, 1)
	}
	px(600, 470, 1)
	rects, _ = d.Flush()
	want := []Rect{{64, 96, 128, 48, 0}, {576, 464, 64, 16, 128 * 48 * 4}}
	if len(rects) != len(want) {
		t.Fatalf("rects %+v, want %+v", rects, want)
	}
	for i := range want {
		if rects[i] != want[i] {
			t.Errorf("rect %d = %+v, want %+v", i, rects[i], want[i])
		}
	}

	d.Invalidate()
	if rects, _ := d.Flush(); len(rects) != 1 || rects[0].W != w || rects[0].H != h {
		t.Fatalf("after Invalidate: %+v", rects)
	}
}
