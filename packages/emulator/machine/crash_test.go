//go:build boot

package machine_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
)

// guestVM drives a booted guest for tests.
type guestVM struct {
	t   *testing.T
	m   *machine.Machine
	out bytes.Buffer
}

func bootGuest(t *testing.T, disk *devices.Disk) *guestVM {
	t.Helper()
	dir := os.Getenv("GUEST_OUT")
	if dir == "" {
		dir = "../../../guest/out"
	}
	kernel, err := os.ReadFile(filepath.Join(dir, "Image"))
	if err != nil {
		t.Skipf("guest not built: %v", err)
	}
	initrd, err := os.ReadFile(filepath.Join(dir, "initrd.cpio"))
	if err != nil {
		t.Fatal(err)
	}
	m := machine.New(machine.Config{})
	if err := m.LoadKernel(kernel); err != nil {
		t.Fatal(err)
	}
	m.LoadInitrd(initrd)
	m.AttachDisk(disk)
	if err := m.Reset(); err != nil {
		t.Fatal(err)
	}
	g := &guestVM{t: t, m: m}
	g.waitFor("root@bootlet:~# ", 2*time.Minute)
	return g
}

// run executes the guest for d of wall-clock time.
func (g *guestVM) run(d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		res := g.m.Run(20 * time.Millisecond)
		g.out.Write(console(g.m))
		if res.Idle {
			time.Sleep(time.Millisecond)
		}
	}
}

func (g *guestVM) waitFor(s string, timeout time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(g.out.String(), s) {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %q; output:\n%s", s, g.out.String())
		}
		g.run(20 * time.Millisecond)
	}
}

// TestCrashConsistency models closing or reloading the page while the guest
// writes: the browser persists the disk's dirty blocks in periodic batches,
// so after a "crash" the disk holds the device state as of the last batch.
// The filesystem must come back without errors.
func TestCrashConsistency(t *testing.T) {
	for trial := 0; trial < 4; trial++ {
		disk := devices.NewDisk(1 << 30)
		disk.Format("test")
		saved := map[uint64][]byte{}
		persist := func() {
			idx, data := disk.TakeDirty()
			for i, b := range idx {
				saved[b] = data[i]
			}
		}
		persist()

		g := bootGuest(t, disk)
		// Constant metadata churn in /root, like a busy shell.
		g.m.Input([]byte("i=0; while true; do i=$((i+1)); echo $i > /root/f$i; mkdir -p /root/d$i/x; " +
			"rm -rf /root/f$((i-3)) /root/d$((i-3)); done &\n"))
		for n := 0; n < 3+trial; n++ {
			g.run(150 * time.Millisecond)
			persist()
		}
		g.run(time.Duration(37*(trial+1)) * time.Millisecond) // unsaved writes are lost

		// "Reload": boot from what was persisted.
		restored := devices.NewDisk(1 << 30)
		for b, data := range saved {
			restored.Restore(b, data)
		}
		h := bootGuest(t, restored)
		h.m.Input([]byte("ls -laR /root >/dev/null; find / -xdev >/dev/null; touch /root/new && echo che''cked\n"))
		h.waitFor("checked", time.Minute)
		if strings.Contains(h.out.String(), "EXT4-fs error") || strings.Contains(h.out.String(), "consistency") {
			t.Fatalf("trial %d: filesystem corrupted after crash:\n%s", trial, tail(h.out.String(), 1500))
		}
		fmt.Printf("trial %d: clean\n", trial)
	}
}

// console returns the guest's new console output and, like a real terminal,
// answers cursor-position queries (the login shell runs resize, which asks
// for one) with an 80x24 size.
func console(m *machine.Machine) []byte {
	out := m.Output()
	if bytes.Contains(out, []byte("\x1b[6n")) {
		m.Input([]byte("\x1b[24;80R"))
	}
	return out
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
