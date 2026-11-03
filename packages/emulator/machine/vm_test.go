//go:build boot

package machine_test

import (
	"bytes"
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

// guest is a booted machine driven from a test: shell commands on the
// serial console, and files through the agent.
type guest struct {
	t       testing.TB
	m       *machine.Machine
	out     bytes.Buffer // console output not yet consumed by waitFor
	instret uint64
	busy    time.Duration // time spent inside Run
	agent   *agent.Client
	dec     agent.Decoder
	ready   bool // agent announced itself
}

func guestImages(t testing.TB) (kernel, initrd []byte) {
	dir := os.Getenv("GUEST_OUT")
	if dir == "" {
		dir = "../../../guest/out"
	}
	kernel, err := os.ReadFile(filepath.Join(dir, "Image"))
	if err != nil {
		t.Skipf("guest not built: %v", err)
	}
	initrd, err = os.ReadFile(filepath.Join(dir, "initrd.cpio"))
	if err != nil {
		t.Fatal(err)
	}
	return kernel, initrd
}

// newGuest creates a machine with a fresh disk and the agent port, and boots
// it. setup runs before the first reset to attach extra devices.
func newGuest(t testing.TB, cfg machine.Config, setup func(m *machine.Machine)) *guest {
	kernel, initrd := guestImages(t)
	disk := devices.NewDisk(256 << 20)
	disk.Format("test")
	m := machine.New(cfg)
	if err := m.LoadKernel(kernel); err != nil {
		t.Fatal(err)
	}
	m.LoadInitrd(initrd)
	m.AttachDisk(disk)
	m.AttachAgent()
	if setup != nil {
		setup(m)
	}
	if err := m.Reset(); err != nil {
		t.Fatal(err)
	}
	g := &guest{t: t, m: m}
	g.agent = &agent.Client{
		Send: m.AgentInput,
		Recv: func() ([]byte, error) {
			deadline := time.Now().Add(5 * time.Minute)
			for time.Now().Before(deadline) {
				if b := m.AgentOutput(); len(b) > 0 {
					return b, nil
				}
				g.step()
			}
			return nil, errors.New("agent did not answer; console:\n" + tail(g.out.String(), 2000))
		},
	}
	return g
}

// step runs the machine for a short slice, sleeping while it idles.
func (g *guest) step() {
	start := time.Now()
	res := g.m.Run(20 * time.Millisecond)
	g.busy += time.Since(start)
	g.instret += res.Instructions
	g.out.Write(console(g.m))
	if res.Halted {
		g.t.Fatalf("guest halted; console:\n%s", tail(g.out.String(), 4000))
	}
	if res.Idle {
		wait := 5 * time.Millisecond
		if res.IdleFor >= 0 {
			wait = min(res.IdleFor, wait)
		}
		time.Sleep(wait)
	}
}

// waitFor runs the machine until the console shows s, and returns the
// output up to and including it.
func (g *guest) waitFor(s string, timeout time.Duration) string {
	g.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if i := strings.Index(g.out.String(), s); i >= 0 {
			got := g.out.String()[:i+len(s)]
			rest := g.out.String()[i+len(s):]
			g.out.Reset()
			g.out.WriteString(rest)
			return got
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %q; output:\n%s", s, tail(g.out.String(), 4000))
		}
		g.step()
	}
}

const prompt = "root@bootlet:~# "

// sh runs a shell command and returns its output (after the echoed command
// line, up to the end marker).
func (g *guest) sh(cmd string, timeout time.Duration) string {
	g.t.Helper()
	g.out.Reset()
	g.m.Input([]byte(cmd + "; echo __e''nd__\n"))
	got := g.waitFor("__end__", timeout)
	g.waitFor(prompt, timeout)
	if i := strings.Index(got, "\n"); i >= 0 {
		got = got[i+1:] // the echoed command line
	}
	return strings.TrimSuffix(got, "__end__")
}

// agentReady waits until bootlet-agent has announced itself.
func (g *guest) agentReady() {
	g.t.Helper()
	for !g.ready {
		b, err := g.agent.Recv()
		if err != nil {
			g.t.Fatal(err)
		}
		g.dec.Feed(b)
		if _, _, ok := g.dec.Next(); ok {
			g.ready = true
		}
	}
}

// upload writes a file into the guest through the agent.
func (g *guest) upload(path string, data []byte) {
	g.t.Helper()
	g.agentReady()
	if _, _, err := g.agent.Call(agent.Request{Op: "mkdir", Path: filepath.Dir(path)}, nil); err != nil {
		g.t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := g.agent.WriteFile(path, data); err != nil {
		g.t.Fatalf("upload %s: %v", path, err)
	}
}
