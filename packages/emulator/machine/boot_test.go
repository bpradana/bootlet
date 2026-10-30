//go:build boot

package machine_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
)

// TestBoot boots the packed guest (mise run guest:fetch) and runs commands
// in its shell. Run with: go test -tags boot -run TestBoot ./machine
func TestBoot(t *testing.T) {
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

	disk := devices.NewDisk(16 << 20)
	disk.FormatExt2("test")
	m := machine.New(machine.Config{})
	if err := m.LoadKernel(kernel); err != nil {
		t.Fatal(err)
	}
	m.LoadInitrd(initrd)
	m.AttachDisk(disk)
	if err := m.Reset(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	var instret uint64
	start := time.Now()
	// waitFor runs the machine until the console output contains s.
	waitFor := func(s string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for !strings.Contains(out.String(), s) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q; output:\n%s", s, out.String())
			}
			res := m.Run(20 * time.Millisecond)
			instret += res.Instructions
			out.Write(m.Output())
			if res.Halted {
				t.Fatalf("guest halted; output:\n%s", out.String())
			}
			if res.Idle {
				wait := 10 * time.Millisecond
				if res.IdleFor >= 0 {
					wait = min(res.IdleFor, wait)
				}
				time.Sleep(wait)
			}
		}
	}

	waitFor("root@bootlet:~# ", 2*time.Minute)
	boot := time.Since(start)
	t.Logf("shell prompt after %v (%.1f MIPS)", boot.Round(time.Millisecond),
		float64(instret)/boot.Seconds()/1e6)

	out.Reset()
	m.Input([]byte("uname -sm; ls /; echo hi > /tmp/f; cat /tmp/f; echo do''ne\n"))
	waitFor("done", time.Minute)
	got := out.String()
	for _, want := range []string{"Linux riscv64", "bin", "proc", "hi\r\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	// The persistent disk is mounted at /data in the background; a file
	// written there must survive a reboot.
	out.Reset()
	m.Input([]byte("while ! mountpoint -q /data; do sleep 0.2; done; echo saved > /data/f; sync; echo wr''ote\n"))
	waitFor("wrote", time.Minute)
	out.Reset()
	m.Input([]byte("reboot\n"))
	waitFor("root@bootlet:~# ", 2*time.Minute)
	out.Reset()
	m.Input([]byte("while ! mountpoint -q /data; do sleep 0.2; done; cat /data/f; echo re''ad\n"))
	waitFor("read", time.Minute)
	if !strings.Contains(out.String(), "saved") {
		t.Errorf("file in /data did not survive reboot:\n%s", out.String())
	}

	out.Reset()
	m.Input([]byte("poweroff\n"))
	deadline := time.Now().Add(time.Minute)
	for !m.Halted() {
		if time.Now().After(deadline) {
			t.Fatalf("poweroff did not halt; output:\n%s", out.String())
		}
		m.Run(20 * time.Millisecond)
		out.Write(m.Output())
	}
}
