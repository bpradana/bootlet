package devices

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// Splash is bootlet's power-on screen, drawn by the emulator straight into the
// guest framebuffer before Linux runs: the >_ mark, the wordmark, a BIOS-style
// summary of the machine and an animated bar with the time since reset. No
// firmware is involved.
//
// The splash only ever draws while the framebuffer still holds exactly what it
// drew last. The first pixel the guest changes (simpledrm/fbcon taking over
// the display) ends it for good, so it never covers guest output.
type Splash struct {
	d                     *Display
	fb                    []byte
	width, height, stride int
	img                   []byte // what the splash last drew
	info                  string
	frame                 int
	done                  bool
}

// Splash palette (x8r8g8b8).
const (
	splBG     = 0x0d1117
	splPanel  = 0x161b22
	splTrack  = 0x21262d
	splText   = 0xe6edf3
	splMuted  = 0x8b949e
	splGreen  = 0x3fb950
	splGreenD = 0x1f5f2b
)

// NewSplash draws the splash on d. info is the machine summary line, e.g.
// "RV64GC  128 MIB RAM  640X480 DISPLAY".
func NewSplash(d *Display, info string) *Splash {
	s := &Splash{d: d, fb: d.fb, width: d.Width, height: d.Height, stride: d.Stride, info: info}
	s.img = make([]byte, len(s.fb))
	s.drawStatic()
	s.drawDynamic(0)
	copy(s.fb, s.img)
	s.d.Damage()
	return s
}

// Done reports whether the guest has taken over the display.
func (s *Splash) Done() bool { return s.done }

// Tick animates the splash for the time since reset. It returns false, and
// draws nothing, once the guest has written to the framebuffer.
func (s *Splash) Tick(elapsed time.Duration) bool {
	if s.done {
		return false
	}
	if !bytes.Equal(s.fb, s.img) {
		s.done = true
		return false
	}
	s.frame++
	s.drawDynamic(elapsed)
	copy(s.fb, s.img)
	s.d.Damage()
	return true
}

// ---- drawing into s.img ----

func (s *Splash) px(x, y int, c uint32) {
	if x >= 0 && y >= 0 && x < s.width && y < s.height {
		binary.LittleEndian.PutUint32(s.img[y*s.stride+x*4:], c)
	}
}

func (s *Splash) rect(x, y, w, h int, c uint32) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			s.px(i, j, c)
		}
	}
}

// roundRect fills a rectangle with corners of radius r.
func (s *Splash) roundRect(x, y, w, h, r int, c uint32) {
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			dx := max(r-i-1, i-(w-r), 0)
			dy := max(r-j-1, j-(h-r), 0)
			if dx*dx+dy*dy <= r*r {
				s.px(x+i, y+j, c)
			}
		}
	}
}

// stroke draws a line of the given thickness (a square brush along it).
func (s *Splash) stroke(x0, y0, x1, y1, width int, c uint32) {
	n := max(abs(x1-x0), abs(y1-y0))
	for k := 0; k <= n; k++ {
		x := x0 + (x1-x0)*k/max(n, 1)
		y := y0 + (y1-y0)*k/max(n, 1)
		s.rect(x-width/2, y-width/2, width, width, c)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// text draws s in the 5x7 font at the given scale; it returns the width.
func (s *Splash) text(x, y, scale int, str string, c uint32) int {
	for i, r := range str {
		g, ok := font5x7[r]
		if !ok {
			continue
		}
		for row := 0; row < 7; row++ {
			for col := 0; col < 5; col++ {
				if g[row][col] == '#' {
					s.rect(x+i*6*scale+col*scale, y+row*scale, scale, scale, c)
				}
			}
		}
	}
	return textWidth(str, scale)
}

func textWidth(str string, scale int) int {
	n := len([]rune(str))
	if n == 0 {
		return 0
	}
	return (n*6 - 1) * scale
}

// Layout.
const (
	markSize = 96
	markY    = 96
	wordY    = 212
	tagY     = 264
	barY     = 304
	barW     = 200
	barH     = 4
	infoY    = 412
	statusY  = 440
	margin   = 24
)

func (s *Splash) drawStatic() {
	for i := 0; i < len(s.img); i += 4 {
		binary.LittleEndian.PutUint32(s.img[i:], splBG)
	}
	cx := s.width / 2

	// The >_ mark, as in the page's icon (a 16-unit grid scaled by 6).
	mx, my := cx-markSize/2, markY
	s.roundRect(mx, my, markSize, markSize, 18, splPanel)
	u := markSize / 16
	s.stroke(mx+4*u, my+5*u, mx+7*u, my+8*u, 10, splGreen)
	s.stroke(mx+7*u, my+8*u, mx+4*u, my+11*u, 10, splGreen)
	s.stroke(mx+9*u, my+11*u, mx+12*u+u/2, my+11*u, 10, splText)

	word := "BOOTLET"
	s.text(cx-textWidth(word, 5)/2, wordY, 5, word, splText)
	tag := "RISC-V LINUX IN YOUR BROWSER"
	s.text(cx-textWidth(tag, 2)/2, tagY, 2, tag, splMuted)

	// BIOS-style summary at the bottom left, under a rule.
	s.rect(margin, infoY-16, s.width-2*margin, 1, splTrack)
	s.text(margin, infoY, 2, s.info, splMuted)
}

// drawDynamic redraws the bar and the status line.
func (s *Splash) drawDynamic(elapsed time.Duration) {
	cx := s.width / 2
	bx := cx - barW/2
	s.rect(bx, barY, barW, barH, splTrack)
	// A segment sweeping back and forth across the track.
	const seg = 56
	period := 2 * (barW - seg)
	p := (s.frame * 8) % period
	if p > barW-seg {
		p = period - p
	}
	s.rect(bx+p, barY, seg, barH, splGreen)
	s.rect(bx+p-6, barY, 6, barH, splGreenD)
	s.rect(bx+p+seg, barY, 6, barH, splGreenD)

	s.rect(margin, statusY, s.width-2*margin, 7*2, splBG)
	status := fmt.Sprintf("STARTING LINUX %d.%d S", int(elapsed/time.Second), int(elapsed%time.Second/(100*time.Millisecond)))
	w := s.text(margin, statusY, 2, status, splText)
	// A blinking block cursor after the text.
	if s.frame/4%2 == 0 {
		s.rect(margin+w+8, statusY, 10, 14, splGreen)
	}
}

// font5x7 covers the characters the splash uses.
var font5x7 = map[rune][7]string{
	' ': {".....", ".....", ".....", ".....", ".....", ".....", "....."},
	'A': {".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'B': {"####.", "#...#", "#...#", "####.", "#...#", "#...#", "####."},
	'C': {".###.", "#...#", "#....", "#....", "#....", "#...#", ".###."},
	'D': {"####.", "#...#", "#...#", "#...#", "#...#", "#...#", "####."},
	'E': {"#####", "#....", "#....", "####.", "#....", "#....", "#####"},
	'F': {"#####", "#....", "#....", "####.", "#....", "#....", "#...."},
	'G': {".###.", "#...#", "#....", "#.###", "#...#", "#...#", ".###."},
	'H': {"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'I': {".###.", "..#..", "..#..", "..#..", "..#..", "..#..", ".###."},
	'J': {"..###", "...#.", "...#.", "...#.", "...#.", "#..#.", ".##.."},
	'K': {"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"},
	'L': {"#....", "#....", "#....", "#....", "#....", "#....", "#####"},
	'M': {"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"},
	'N': {"#...#", "#...#", "##..#", "#.#.#", "#..##", "#...#", "#...#"},
	'O': {".###.", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'P': {"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."},
	'Q': {".###.", "#...#", "#...#", "#...#", "#.#.#", "#..#.", ".##.#"},
	'R': {"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"},
	'S': {".####", "#....", "#....", ".###.", "....#", "....#", "####."},
	'T': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'U': {"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'V': {"#...#", "#...#", "#...#", "#...#", "#...#", ".#.#.", "..#.."},
	'W': {"#...#", "#...#", "#...#", "#.#.#", "#.#.#", "#.#.#", ".#.#."},
	'X': {"#...#", "#...#", ".#.#.", "..#..", ".#.#.", "#...#", "#...#"},
	'Y': {"#...#", "#...#", ".#.#.", "..#..", "..#..", "..#..", "..#.."},
	'Z': {"#####", "....#", "...#.", "..#..", ".#...", "#....", "#####"},
	'0': {".###.", "#...#", "#..##", "#.#.#", "##..#", "#...#", ".###."},
	'1': {"..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."},
	'2': {".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"},
	'3': {"#####", "...#.", "..#..", "...#.", "....#", "#...#", ".###."},
	'4': {"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."},
	'5': {"#####", "#....", "####.", "....#", "....#", "#...#", ".###."},
	'6': {"..##.", ".#...", "#....", "####.", "#...#", "#...#", ".###."},
	'7': {"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
	'8': {".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."},
	'9': {".###.", "#...#", "#...#", ".####", "....#", "...#.", ".##.."},
	'.': {".....", ".....", ".....", ".....", ".....", ".##..", ".##.."},
	':': {".....", ".##..", ".##..", ".....", ".##..", ".##..", "....."},
	'-': {".....", ".....", ".....", "#####", ".....", ".....", "....."},
	'/': {"....#", "....#", "...#.", "..#..", ".#...", "#....", "#...."},
}
