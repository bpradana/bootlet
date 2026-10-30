package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"bootlet/shared/wisp"
)

// echoServer accepts TCP connections on loopback and echoes what it reads.
func echoServer(t *testing.T) uint16 {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

// session drives a wisp.Client over a real WebSocket to the relay.
type session struct {
	t   *testing.T
	ctx context.Context
	ws  *websocket.Conn
	c   *wisp.Client
}

func dial(t *testing.T, cfg Config) *session {
	srv := httptest.NewServer(NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &session{t: t, ctx: ctx, ws: ws, c: wisp.NewClient()}
}

func (s *session) flush() {
	for _, pkt := range s.c.Outgoing() {
		if err := s.ws.Write(s.ctx, websocket.MessageBinary, pkt); err != nil {
			s.t.Fatal(err)
		}
	}
}

// next returns the next stream event, handling CONTINUE packets on the way.
func (s *session) next() wisp.Event {
	for {
		s.flush()
		_, msg, err := s.ws.Read(s.ctx)
		if err != nil {
			s.t.Fatal(err)
		}
		if ev, ok := s.c.Receive(msg); ok {
			return ev
		}
	}
}

func TestRelayEcho(t *testing.T) {
	port := echoServer(t)
	s := dial(t, Config{AllowPrivate: true})
	id := s.c.Connect("127.0.0.1", port)
	s.c.Send(id, []byte("ping"))
	var got []byte
	for len(got) < 4 {
		ev := s.next()
		if ev.Type != wisp.EventData || ev.Stream != id {
			t.Fatalf("unexpected event %+v", ev)
		}
		got = append(got, ev.Data...)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q", got)
	}
}

func TestRelayBlocksPrivateAndPorts(t *testing.T) {
	port := echoServer(t)
	s := dial(t, Config{Ports: map[uint16]bool{port: true}})
	s.c.Connect("127.0.0.1", port)
	if ev := s.next(); ev.Type != wisp.EventClosed || ev.Reason != wisp.CloseBlocked {
		t.Fatalf("loopback not blocked: %+v", ev)
	}
	s.c.Connect("example.com", port+1)
	if ev := s.next(); ev.Type != wisp.EventClosed || ev.Reason != wisp.CloseBlocked {
		t.Fatalf("disallowed port not blocked: %+v", ev)
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "::1": false, "fd00::1": false, "fe80::1": false, "0.0.0.0": false,
	} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("public(%s) = %v, want %v", addr, got, want)
		}
	}
}
