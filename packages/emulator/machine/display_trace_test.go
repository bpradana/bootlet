//go:build boot

package machine_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
)

// TestDisplayTrace measures how many distinct, complete frames reach the
// host display while the guest draws, natively and without a browser. It
// paces Machine.Run and Display.Flush like apps/frontend/src/worker.ts
// (12 ms slices, a flush at the end of a slice at most every 16 ms), then
// runs fbtick (stamped frames at a known rate) and Doom with forward held.
// Diagnostic only; run with:
//
//	BOOTLET_TRACE=1 go test -tags boot -run TestDisplayTrace -v -timeout 30m ./machine
func TestDisplayTrace(t *testing.T) {
	if os.Getenv("BOOTLET_TRACE") == "" {
		t.Skip("set BOOTLET_TRACE=1 to run the display trace")
	}
	for _, dev := range []string{machine.DisplayFramebuffer, machine.DisplayVirtioGPU} {
		t.Run(dev, func(t *testing.T) {
			g := newGuest(t, machine.Config{Display: true, DisplayDevice: dev}, func(m *machine.Machine) { m.AttachKeyboard() })
			g.waitFor(prompt, 5*time.Minute)
			d := g.m.Display

			run := func(name, cmd string, secs int, during func()) {
				g.out.Reset()
				d.Flush()
				tr := devices.NewDisplayTrace(100000)
				d.Trace = tr
				g.m.Input([]byte(cmd + "\n"))
				start, last := time.Now(), time.Time{}
				for time.Since(start) < time.Duration(secs)*time.Second {
					res := g.m.Run(12 * time.Millisecond)
					g.out.Write(console(g.m))
					if time.Since(last) >= 16*time.Millisecond {
						d.Flush()
						last = time.Now()
					}
					if during != nil {
						during()
					}
					if res.Idle {
						time.Sleep(2 * time.Millisecond)
					}
				}
				d.Trace = nil
				report(t, name, tr, time.Since(start))
				t.Logf("%s console:\n%s", name, tail(g.out.String(), 1500))
			}

			// Let fbtick render its frames first (slow under emulation).
			g.sh("fbtick 1 1 >/dev/null", 10*time.Minute)
			run("fbtick-10fps", "fbtick 10 8", 12, nil)
			run("fbtick-30fps", "fbtick 30 8", 12, nil)

			held := false
			run("doom", "DOOM_FPS=1 doom", 30, func() {
				if !held && strings.Contains(g.out.String(), "I_InitGraphics") {
					g.m.KeyEvent(103, true) // KEY_UP
					held = true
				}
			})
			g.m.KeyEvent(103, false)
			g.m.Input([]byte{3}) // Ctrl-C
			for i := 0; i < 100; i++ {
				g.step()
			}
			t.Logf("doom exit:\n%s", tail(g.out.String(), 800))
		})
	}
}

// report logs the trace's aggregates and a compact per-flush timeline.
func report(t *testing.T, name string, tr *devices.DisplayTrace, wall time.Duration) {
	recs, dropped := tr.TakeRecords()
	s := wall.Seconds()
	t.Logf("%s over %.1fs: flushes %.1f/s, changed %.1f/s, >half-screen changes %.1f/s, mean dirty tiles %.0f/changed flush, "+
		"fbtick complete %.1f/s torn %.1f/s skipped %d, gpu flushes %.1f/s transfers %.1f/s (records dropped %d)",
		name, s, float64(tr.Calls)/s, float64(tr.Changed)/s, float64(tr.FullChanges)/s,
		float64(tr.DirtyTiles)/float64(max(tr.Changed, 1)),
		float64(tr.Complete)/s, float64(tr.Torn)/s, tr.Skipped,
		float64(tr.GPUFlushes)/s, float64(tr.GPUTransfers)/s, dropped)
	var b strings.Builder
	prev := time.Duration(0)
	for i, r := range recs {
		if i >= 120 {
			fmt.Fprintf(&b, "... %d more\n", len(recs)-i)
			break
		}
		stamp := ""
		if r.Top >= 0 || r.Bottom >= 0 {
			stamp = fmt.Sprintf(" stamp %d/%d", r.Top, r.Bottom)
		}
		fmt.Fprintf(&b, "%7.3fs +%4dms rows %3d-%3d tiles %3d gpu %d%s\n",
			r.At.Seconds(), (r.At - prev).Milliseconds(), r.MinY, r.MaxY, r.Tiles, r.GPUFlushes, stamp)
		prev = r.At
	}
	t.Logf("%s timeline:\n%s", name, b.String())
}
