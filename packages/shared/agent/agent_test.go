package agent

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDecoderResync(t *testing.T) {
	var d Decoder
	f1 := Encode(Request{ID: 1, Op: "hello"}, nil)
	f2 := Encode(Request{ID: 2, Op: "write", Path: "/x"}, []byte("data"))
	// Garbage, a frame cut short (a previous agent's), then two frames fed
	// one byte at a time.
	stream := append([]byte("junkBLA1\xff\xff\xff\xff"), f1[:7]...)
	stream = append(stream, f1...)
	stream = append(stream, f2...)
	var got []string
	for _, b := range stream {
		d.Feed([]byte{b})
		for {
			hdr, payload, ok := d.Next()
			if !ok {
				break
			}
			got = append(got, string(hdr)+"|"+string(payload))
		}
	}
	want := []string{
		`{"id":1,"op":"hello"}|`,
		`{"id":2,"op":"write","path":"/x"}|data`,
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("frames:\n%q\nwant:\n%q", got, want)
	}
}

type pipeRW struct {
	io.Reader
	io.Writer
}

// serve starts a server on a pipe and returns a client connected to it.
func serve(t *testing.T) *Client {
	toAgent, agentIn := io.Pipe()
	fromAgent, agentOut := io.Pipe()
	go Serve(pipeRW{toAgent, agentOut})
	// Pipes are unbuffered, unlike the console port: send from a goroutine
	// so the client never waits for the server to read.
	sends := make(chan []byte, 64)
	go func() {
		for b := range sends {
			agentIn.Write(b)
		}
	}()
	t.Cleanup(func() { close(sends); agentIn.Close(); fromAgent.Close() })
	buf := make([]byte, 64<<10)
	return &Client{
		Send: func(b []byte) { sends <- b },
		Recv: func() ([]byte, error) {
			n, err := fromAgent.Read(buf)
			return bytes.Clone(buf[:n]), err
		},
	}
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestServe(t *testing.T) {
	c := serve(t)
	dir := t.TempDir()
	p := func(s string) string { return filepath.Join(dir, s) }

	rep, _, err := c.Call(Request{Op: "hello"}, nil)
	if err != nil || rep.Version != Version || rep.Home != Home {
		t.Fatalf("hello: %+v %v", rep, err)
	}

	// A file larger than a chunk round-trips.
	data := make([]byte, 3*ChunkSize+123)
	rand.Read(data)
	if err := c.WriteFile(p("big.bin"), data); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadFile(p("big.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes (%v), want %d", len(got), err, len(data))
	}
	// Rewriting truncates.
	if err := c.WriteFile(p("big.bin"), []byte("short")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p("big.bin")); string(got) != "short" {
		t.Errorf("after rewrite: %q", got)
	}
	if got, err := c.ReadFile(p("empty")); code(err) != "notexist" {
		t.Errorf("missing file: %q %v", got, err)
	}
	c.WriteFile(p("empty"), nil)
	if got, err := c.ReadFile(p("empty")); err != nil || len(got) != 0 {
		t.Errorf("empty file: %q %v", got, err)
	}

	if _, _, err := c.Call(Request{Op: "mkdir", Path: p("a/b")}, nil); err != nil {
		t.Fatal(err)
	}
	os.Symlink("a", p("link"))
	rep, _, err = c.Call(Request{Op: "list", Path: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range rep.Entries {
		names = append(names, e.Name+":"+e.Type)
	}
	if want := "[a:dir link:link big.bin:file empty:file]"; fmtList(names) != want {
		t.Errorf("list: %v, want %s", names, want)
	}
	if l := rep.Entries[1]; l.Target != "a" || !l.TargetDir {
		t.Errorf("link entry %+v", l)
	}

	// rename refuses to replace unless asked, and refuses moving into itself.
	if _, _, err := c.Call(Request{Op: "rename", Path: p("empty"), To: p("big.bin")}, nil); code(err) != "exist" {
		t.Errorf("rename onto existing: %v", err)
	}
	if _, _, err := c.Call(Request{Op: "rename", Path: p("empty"), To: p("big.bin"), Overwrite: true}, nil); err != nil {
		t.Errorf("rename with overwrite: %v", err)
	}
	if _, _, err := c.Call(Request{Op: "rename", Path: p("a"), To: p("a/b/a")}, nil); err == nil {
		t.Error("moved a directory into itself")
	}
	if _, _, err := c.Call(Request{Op: "copy", Path: p("a"), To: p("c")}, nil); err != nil {
		t.Error(err)
	}
	if fi, err := os.Stat(p("c/b")); err != nil || !fi.IsDir() {
		t.Errorf("copied tree: %v", err)
	}
	if _, _, err := c.Call(Request{Op: "remove", Path: p("a")}, nil); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(p("a")); !os.IsNotExist(err) {
		t.Error("directory not removed")
	}
	if _, _, err := c.Call(Request{Op: "remove", Path: p("nothing")}, nil); code(err) != "notexist" {
		t.Errorf("removing a missing path: %v", err)
	}
	if _, _, err := c.Call(Request{Op: "list", Path: "relative"}, nil); err == nil {
		t.Error("accepted a relative path")
	}
	rep, _, err = c.Call(Request{Op: "stat", Path: p("big.bin")}, nil)
	if err != nil || rep.Entry.Size != 0 || rep.Entry.Type != "file" {
		t.Errorf("stat: %+v %v", rep.Entry, err)
	}
}

func fmtList(s []string) string {
	out := "["
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out + "]"
}

func TestParseStats(t *testing.T) {
	cpu := parseCPU([]byte("cpu  100 5 30 800 20 1 2 0 0 0\ncpu0 100 5 30 800 20 1 2 0 0 0\n"))
	if want := (CPUTimes{User: 105, System: 33, IOWait: 20, Idle: 800}); cpu != want {
		t.Errorf("cpu %+v, want %+v", cpu, want)
	}
	mem := parseMeminfo([]byte("MemTotal:  120000 kB\nMemFree:  30000 kB\nMemAvailable:  70000 kB\nCached: 1 kB\n"))
	if mem != (Mem{Total: 120000 << 10, Free: 30000 << 10, Available: 70000 << 10}) {
		t.Errorf("mem %+v", mem)
	}
	dev := "Inter-|   Receive  |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets\n" +
		"    lo:  500 5 0 0 0 0 0 0  500 5 0 0 0 0 0 0\n  eth0: 12345 10 0 0 0 0 0 0 678 9 0 0 0 0 0 0\n"
	if n := parseNetDev([]byte(dev), "eth0"); n == nil || *n != (Net{RX: 12345, TX: 678}) {
		t.Errorf("net %+v", n)
	}
	if parseNetDev([]byte(dev), "eth1") != nil {
		t.Error("found a missing interface")
	}
	p, flags, err := parseProcStat([]byte("42 (cc1 (x) y) R 1 42 42 0 -1 4194304 1 0 0 0 250 50 0 0 20 0 1 0 100 1000000 300 18446744073709551615"))
	if err != nil || p.PID != 42 || p.Name != "cc1 (x) y" || p.Ticks != 300 || p.RSS != 300*uint64(os.Getpagesize()) || flags != 4194304 {
		t.Errorf("proc %+v flags %d %v", p, flags, err)
	}
	if _, flags, _ := parseProcStat([]byte("2 (kthreadd) S 0 0 0 0 -1 2129984 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0 18446744073709551615")); flags&pfKthread == 0 {
		t.Error("kernel thread not recognized")
	}
}
