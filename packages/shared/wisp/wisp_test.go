package wisp

import (
	"bytes"
	"testing"
)

func TestClientFlowControl(t *testing.T) {
	c := NewClient()
	id := c.Connect("example.com", 80)
	c.Send(id, []byte("a"))
	if out := c.Outgoing(); len(out) != 0 {
		t.Fatalf("sent %d packets before the server's initial CONTINUE", len(out))
	}

	c.Receive(Continue(0, 2))
	out := c.Outgoing()
	if len(out) != 2 {
		t.Fatalf("got %d packets after ready, want CONNECT + DATA", len(out))
	}
	p, _ := Decode(out[0])
	typ, port, host, _ := ParseConnect(p.Payload)
	if p.Type != TypeConnect || p.Stream != id || typ != StreamTCP || port != 80 || host != "example.com" {
		t.Fatalf("bad CONNECT: %+v %d %d %q", p, typ, port, host)
	}

	// One credit left: the second Send goes out, the third waits.
	c.Send(id, []byte("b"))
	c.Send(id, []byte("c"))
	if out := c.Outgoing(); len(out) != 1 {
		t.Fatalf("sent %d packets with 1 credit", len(out))
	}
	c.Receive(Continue(id, 5))
	out = c.Outgoing()
	if len(out) != 1 {
		t.Fatalf("queued packet not flushed after CONTINUE")
	}
	if p, _ := Decode(out[0]); p.Type != TypeData || !bytes.Equal(p.Payload, []byte("c")) {
		t.Fatalf("bad flushed packet %+v", p)
	}

	ev, ok := c.Receive(Encode(TypeData, id, []byte("hello")))
	if !ok || ev.Type != EventData || string(ev.Data) != "hello" {
		t.Fatalf("bad data event %+v", ev)
	}
	ev, ok = c.Receive(Close(id, CloseVoluntary))
	if !ok || ev.Type != EventClosed || ev.Reason != CloseVoluntary {
		t.Fatalf("bad close event %+v", ev)
	}
	if _, ok := c.Receive(Encode(TypeData, id, []byte("late"))); ok {
		t.Fatal("data delivered for a closed stream")
	}
}
