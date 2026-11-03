package devices

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSplash(t *testing.T) {
	const w, h, stride = 640, 480, 640 * 4
	fb := make([]byte, stride*h)
	d := NewDisplay(fb, w, h, stride)
	s := NewSplash(d, "RV64GC  128 MIB RAM  640X480 DISPLAY")
	if bytes.Equal(fb, make([]byte, len(fb))) {
		t.Fatal("splash drew nothing")
	}
	d.Flush()

	// Animation changes only the bar and status line.
	before := append([]byte(nil), fb...)
	if !s.Tick(1500 * time.Millisecond) {
		t.Fatal("splash ended without guest output")
	}
	rects, _ := d.Flush()
	if len(rects) == 0 {
		t.Fatal("tick changed nothing")
	}
	area := 0
	for _, r := range rects {
		area += r.W * r.H
	}
	if area > w*h/4 {
		t.Errorf("a tick redraws %d pixels (%+v); it should only touch the animated parts", area, rects)
	}
	if bytes.Equal(before, fb) {
		t.Fatal("animation did not change the framebuffer")
	}

	if dir := os.Getenv("BOOTLET_SCREENSHOT"); dir != "" {
		f, _ := os.Create(filepath.Join(dir, "splash.png"))
		png.Encode(f, &image.NRGBA{Pix: d.Snapshot(), Stride: w * 4, Rect: image.Rect(0, 0, w, h)})
		f.Close()
	}

	// The guest draws one pixel: the splash stops and leaves it alone.
	fb[100*stride+100*4] ^= 0xff
	guest := append([]byte(nil), fb...)
	if s.Tick(1600 * time.Millisecond) {
		t.Fatal("splash kept animating after the guest drew")
	}
	if !s.Done() || !bytes.Equal(fb, guest) {
		t.Fatal("splash drew over guest output")
	}
}
