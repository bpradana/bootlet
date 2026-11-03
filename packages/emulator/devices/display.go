package devices

import (
	"bytes"
	"encoding/binary"
)

// Display presents a linear framebuffer in guest memory (described to the
// guest as a "simple-framebuffer", pixel format x8r8g8b8). The guest writes
// pixels with ordinary stores, so nothing on the CPU's hot path tracks them;
// instead Flush compares the framebuffer with a shadow copy of what was last
// presented, tile by tile, and hands over only the changed rectangles.
type Display struct {
	Width, Height, Stride int

	fb     []byte // the guest's framebuffer (a slice of guest RAM)
	shadow []byte // what the host has last been given
	rgba   []byte // reused output buffer
	rects  []Rect
	dirty  []bool // per tile, for the current flush
	full   bool   // next flush resends everything

	// Frames and Bytes count flushes that changed something and the RGBA
	// bytes they produced, for statistics.
	Frames, Bytes uint64
}

// Rect is a changed region in pixels. Its pixels are at Off in the RGBA data
// returned by Flush, packed row by row (W*4 bytes per row).
type Rect struct {
	X, Y, W, H int
	Off        int
}

const (
	tileW = 64 // pixels
	tileH = 16 // rows
)

func NewDisplay(fb []byte, width, height, stride int) *Display {
	d := &Display{Width: width, Height: height, Stride: stride, fb: fb, shadow: make([]byte, len(fb))}
	d.dirty = make([]bool, d.tilesX()*d.tilesY())
	d.full = true
	return d
}

func (d *Display) tilesX() int { return (d.Width + tileW - 1) / tileW }
func (d *Display) tilesY() int { return (d.Height + tileH - 1) / tileH }

// Framebuffer returns the guest framebuffer memory.
func (d *Display) Framebuffer() []byte { return d.fb }

// Invalidate makes the next Flush return the whole screen, e.g. after the
// host-side canvas was recreated.
func (d *Display) Invalidate() { d.full = true }

// Flush returns the rectangles that changed since the last call and their
// pixels converted to RGBA (alpha 255), or nil if nothing changed. The data
// is only valid until the next call.
func (d *Display) Flush() ([]Rect, []byte) {
	tx, ty := d.tilesX(), d.tilesY()
	any := false
	for y := 0; y < ty; y++ {
		for x := 0; x < tx; x++ {
			ch := d.full || d.tileChanged(x, y)
			d.dirty[y*tx+x] = ch
			any = any || ch
		}
	}
	d.full = false
	if !any {
		return nil, nil
	}
	d.rects = d.rects[:0]
	// Merge horizontal runs of dirty tiles per tile row, then extend a run
	// downwards while the rows below have exactly the same run.
	for y := 0; y < ty; y++ {
		for x := 0; x < tx; {
			if !d.dirty[y*tx+x] {
				x++
				continue
			}
			x0 := x
			for x < tx && d.dirty[y*tx+x] {
				x++
			}
			y1 := y + 1
			for y1 < ty && d.sameRun(y1, x0, x) {
				for i := x0; i < x; i++ {
					d.dirty[y1*tx+i] = false
				}
				y1++
			}
			r := Rect{X: x0 * tileW, Y: y * tileH, W: x*tileW - x0*tileW, H: y1*tileH - y*tileH}
			r.W = min(r.W, d.Width-r.X)
			r.H = min(r.H, d.Height-r.Y)
			d.rects = append(d.rects, r)
		}
	}
	// Convert and copy into the shadow.
	n := 0
	for i := range d.rects {
		d.rects[i].Off = n
		n += d.rects[i].W * d.rects[i].H * 4
	}
	if cap(d.rgba) < n {
		d.rgba = make([]byte, n)
	}
	d.rgba = d.rgba[:n]
	for _, r := range d.rects {
		out := d.rgba[r.Off:]
		for row := 0; row < r.H; row++ {
			o := (r.Y+row)*d.Stride + r.X*4
			src := d.fb[o : o+r.W*4]
			copy(d.shadow[o:], src)
			convertRow(out[row*r.W*4:], src)
		}
	}
	d.Frames++
	d.Bytes += uint64(n)
	return d.rects, d.rgba
}

func (d *Display) sameRun(ty, x0, x1 int) bool {
	tx := d.tilesX()
	if x0 > 0 && d.dirty[ty*tx+x0-1] || x1 < tx && d.dirty[ty*tx+x1] {
		return false
	}
	for i := x0; i < x1; i++ {
		if !d.dirty[ty*tx+i] {
			return false
		}
	}
	return true
}

func (d *Display) tileChanged(tx, ty int) bool {
	x0 := tx * tileW * 4
	x1 := min((tx+1)*tileW, d.Width) * 4
	y1 := min((ty+1)*tileH, d.Height)
	for y := ty * tileH; y < y1; y++ {
		o := y * d.Stride
		if !bytes.Equal(d.fb[o+x0:o+x1], d.shadow[o+x0:o+x1]) {
			return true
		}
	}
	return false
}

// convertRow converts little-endian x8r8g8b8 pixels (bytes B, G, R, X) to
// RGBA with full alpha.
func convertRow(dst, src []byte) {
	le := binary.LittleEndian
	for i := 0; i+4 <= len(src); i += 4 {
		v := le.Uint32(src[i:])
		le.PutUint32(dst[i:], v>>16&0xff|v&0xff00|(v&0xff)<<16|0xff000000)
	}
}

// Snapshot returns the whole screen as RGBA, for screenshots.
func (d *Display) Snapshot() []byte {
	out := make([]byte, d.Width*d.Height*4)
	for y := 0; y < d.Height; y++ {
		convertRow(out[y*d.Width*4:(y+1)*d.Width*4], d.fb[y*d.Stride:y*d.Stride+d.Width*4])
	}
	return out
}
