//go:build boot

package machine_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
	"bootlet/shared/agent"
)

// TestAgent boots the guest and drives bootlet-agent, the file explorer's
// guest program, over the virtio console port: the same requests the page
// makes. Run with: go test -tags boot -run TestAgent ./machine
func TestAgent(t *testing.T) {
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
	disk := devices.NewDisk(256 << 20)
	disk.Format("test")
	m := machine.New(machine.Config{})
	if err := m.LoadKernel(kernel); err != nil {
		t.Fatal(err)
	}
	m.LoadInitrd(initrd)
	m.AttachDisk(disk)
	m.AttachAgent()
	if err := m.Reset(); err != nil {
		t.Fatal(err)
	}

	var con bytes.Buffer
	step := func() {
		res := m.Run(20 * time.Millisecond)
		con.Write(console(m))
		if res.Halted {
			t.Fatalf("guest halted; console:\n%s", con.String())
		}
		if res.Idle {
			wait := 10 * time.Millisecond
			if res.IdleFor >= 0 {
				wait = min(res.IdleFor, wait)
			}
			time.Sleep(wait)
		}
	}
	c := &agent.Client{
		Send: m.AgentInput,
		Recv: func() ([]byte, error) {
			deadline := time.Now().Add(2 * time.Minute)
			for time.Now().Before(deadline) {
				if b := m.AgentOutput(); len(b) > 0 {
					return b, nil
				}
				step()
			}
			return nil, errors.New("agent did not answer; console:\n" + tail(con.String(), 2000))
		},
	}

	// The agent announces itself once init starts it.
	start := time.Now()
	var dec agent.Decoder
	for {
		b, err := c.Recv()
		if err != nil {
			t.Fatal(err)
		}
		dec.Feed(b)
		if hdr, _, ok := dec.Next(); ok {
			if !strings.Contains(string(hdr), `"home":"/root"`) {
				t.Fatalf("first message from the agent: %s", hdr)
			}
			break
		}
	}
	t.Logf("agent ready after %v", time.Since(start).Round(time.Millisecond))

	call := func(req agent.Request) agent.Reply {
		t.Helper()
		rep, _, err := c.Call(req, nil)
		if err != nil {
			t.Fatalf("%s %s: %v", req.Op, req.Path, err)
		}
		return rep
	}
	rep := call(agent.Request{Op: "list", Path: "/"})
	var names []string
	for _, e := range rep.Entries {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, " "); !strings.Contains(got, "bin") || !strings.Contains(got, "root") {
		t.Errorf("listing of /: %s", got)
	}

	data := make([]byte, 1<<20)
	rand.Read(data)
	sum := sha1.Sum(data)
	start = time.Now()
	call(agent.Request{Op: "mkdir", Path: "/root/up/sub"})
	if err := c.WriteFile("/root/up/data.bin", data); err != nil {
		t.Fatal(err)
	}
	up := time.Since(start)
	start = time.Now()
	got, err := c.ReadFile("/root/up/data.bin")
	if err != nil {
		t.Fatal(err)
	}
	down := time.Since(start)
	if !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes that differ from what was written", len(got))
	}
	t.Logf("1 MiB upload %v (%.0f KiB/s), download %v (%.0f KiB/s)",
		up.Round(time.Millisecond), 1024/up.Seconds(), down.Round(time.Millisecond), 1024/down.Seconds())

	// The guest sees the uploaded file.
	for !strings.Contains(con.String(), "root@bootlet:~# ") {
		step()
	}
	con.Reset()
	m.Input([]byte("sha1sum /root/up/data.bin; echo do''ne\n"))
	for !strings.Contains(con.String(), "done") {
		step()
	}
	if want := hex.EncodeToString(sum[:]); !strings.Contains(con.String(), want) {
		t.Errorf("sha1sum in the guest:\n%s\nwant %s", con.String(), want)
	}

	// Moving a directory that only exists in the overlay's lower layer
	// (from the initramfs) needs the copy fallback.
	call(agent.Request{Op: "rename", Path: "/etc/apk", To: "/root/up/sub/apk"})
	if _, _, err := c.Call(agent.Request{Op: "stat", Path: "/etc/apk"}, nil); err == nil {
		t.Error("/etc/apk still exists after the move")
	}
	if rep := call(agent.Request{Op: "stat", Path: "/root/up/sub/apk/arch"}); rep.Entry.Type != "file" {
		t.Errorf("moved directory: %+v", rep.Entry)
	}
	call(agent.Request{Op: "remove", Path: "/root/up"})
	if _, _, err := c.Call(agent.Request{Op: "stat", Path: "/root/up"}, nil); err == nil {
		t.Error("/root/up still exists after remove")
	}
}
