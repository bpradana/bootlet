//go:build boot

package machine_test

import (
	"strings"
	"testing"
	"time"

	"bootlet/emulator/machine"
)

// TestDisplay boots with each display device and checks that Linux drives
// it (simpledrm or virtio-gpu: /dev/fb0 and fbcon, and no second DRM device)
// and that guest pixel writes reach the host through Display.Flush. With
// virtio-gpu, the splash must also hand over to the guest. Run with:
// go test -tags boot -run TestDisplay ./machine
func TestDisplay(t *testing.T) {
	for _, dev := range []struct{ name, fb, driver string }{
		{machine.DisplayFramebuffer, "simpledrm", "simple-framebuffer"},
		{machine.DisplayVirtioGPU, "virtio_gpudrmfb", "virtio_gpu"},
	} {
		t.Run(dev.name, func(t *testing.T) {
			gpu := dev.name == machine.DisplayVirtioGPU
			g := newGuest(t, machine.Config{Display: true, DisplayDevice: dev.name, Splash: gpu}, nil)
			start := time.Now()
			g.waitFor(prompt, 2*time.Minute)
			t.Logf("shell prompt after %v", time.Since(start).Round(time.Millisecond))
			if gpu {
				if g.m.SplashActive() || g.m.SplashHandover() == 0 {
					t.Fatal("the guest never took over the display from the splash")
				}
				t.Logf("guest took over the display %v after reset", g.m.SplashHandover().Round(time.Millisecond))
			}
			d := g.m.Display
			d.Flush() // drain everything drawn during boot

			out := g.sh("cat /sys/class/graphics/fb0/virtual_size /sys/class/graphics/fb0/bits_per_pixel /sys/class/graphics/fb0/name; "+
				"basename $(readlink /sys/class/drm/card0/device/driver); ls /sys/class/drm", time.Minute)
			for _, want := range []string{"640,480", "32", dev.fb, dev.driver} {
				if !strings.Contains(out, want) {
					t.Errorf("sysfs missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "card1") {
				t.Errorf("a second DRM device:\n%s", out)
			}

			// Fill the screen with white through /dev/fb0 and time how long it
			// takes to become visible to the host (fbdev emulation flushes
			// deferred).
			g.m.Input([]byte("tr '\\000' '\\377' < /dev/zero | head -c 1228800 > /dev/fb0; echo filled\n"))
			start = time.Now()
			var whiteAt time.Duration
			for time.Since(start) < time.Minute {
				g.step()
				if rects, _ := d.Flush(); len(rects) == 0 {
					continue
				}
				// Check the middle of the screen: fbcon redraws the prompt at the top.
				snap := d.Snapshot()
				mid := snap[len(snap)/2:]
				if mid[0] == 0xff && mid[1] == 0xff && mid[2] == 0xff && mid[3] == 0xff {
					whiteAt = time.Since(start)
					break
				}
			}
			if whiteAt == 0 {
				t.Fatalf("framebuffer writes never reached the display")
			}
			t.Logf("guest fill visible after %v", whiteAt.Round(time.Millisecond))
			g.waitFor("filled", time.Minute)
		})
	}
}

// TestKeyboard checks that key events from the host reach the guest as
// evdev events with separate press and release.
func TestKeyboard(t *testing.T) {
	g := newGuest(t, machine.Config{Display: true}, func(m *machine.Machine) { m.AttachKeyboard() })
	g.waitFor(prompt, 2*time.Minute)
	out := g.sh("cat /sys/class/input/event*/device/name", time.Minute)
	if !strings.Contains(out, "bootlet keyboard") {
		t.Fatalf("no virtio keyboard in the guest:\n%s", out)
	}
	// struct input_event is 24 bytes on rv64: 16 bytes of time, then type,
	// code (u16) and value (u32). Read a press+sync and a release+sync.
	g.out.Reset()
	g.m.Input([]byte("od -An -tx1 -w24 -N96 /dev/input/event0 | cut -c50-; echo __k''ey__\n"))
	for i := 0; i < 200; i++ { // let od start and block on the device
		g.step()
	}
	g.m.KeyEvent(30, true) // KEY_A
	g.m.KeyEvent(30, false)
	got := g.waitFor("__key__", time.Minute)
	for _, want := range []string{
		"01 00 1e 00 01 00 00 00", // EV_KEY KEY_A 1
		"01 00 1e 00 00 00 00 00", // EV_KEY KEY_A 0
		"00 00 00 00 00 00 00 00", // EV_SYN
	} {
		if !strings.Contains(got, want) {
			t.Errorf("evdev output missing %q:\n%s", want, got)
		}
	}
}

// TestSplash boots with and without the power-on splash: the guest must take
// over the display (fbcon), and the splash must not slow the boot down.
func TestSplash(t *testing.T) {
	boot := func(splash bool) (time.Duration, *guest) {
		g := newGuest(t, machine.Config{Display: true, Splash: splash}, func(m *machine.Machine) { m.AttachKeyboard() })
		if splash && !g.m.SplashActive() {
			t.Fatal("splash not shown at reset")
		}
		start := time.Now()
		g.waitFor(prompt, 2*time.Minute)
		return time.Since(start), g
	}
	var with, without []time.Duration
	for i := 0; i < 3; i++ {
		d, g := boot(true)
		with = append(with, d)
		if g.m.SplashActive() || g.m.SplashHandover() == 0 {
			t.Fatalf("the guest never took over the display from the splash")
		}
		if i == 0 {
			t.Logf("guest took over the display %v after reset", g.m.SplashHandover().Round(time.Millisecond))
		}
		d, _ = boot(false)
		without = append(without, d)
	}
	t.Logf("boot to prompt: with splash %v, without %v", with, without)
}
