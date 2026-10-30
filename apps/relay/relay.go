package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"bootlet/shared/wisp"
)

// Config controls what the relay will connect to.
type Config struct {
	Ports        map[uint16]bool // allowed destination ports; empty allows all
	Origins      []string        // allowed WebSocket Origin host patterns; empty allows all
	AllowPrivate bool            // allow loopback/private/link-local destinations
	MaxStreams   int             // per WebSocket
	Buffer       int             // per-stream DATA packets buffered (Wisp CONTINUE value)
	DialTimeout  time.Duration
}

// Server is a Wisp v1 server: each WebSocket carries many TCP/UDP streams.
type Server struct {
	cfg Config
	log *slog.Logger
}

func NewServer(cfg Config, log *slog.Logger) *Server {
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = 64
	}
	if cfg.Buffer == 0 {
		cfg.Buffer = 128
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	return &Server{cfg: cfg, log: log}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{OriginPatterns: s.cfg.Origins}
	if len(s.cfg.Origins) == 0 {
		opts.InsecureSkipVerify = true // any origin may connect
	}
	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := &conn{srv: s, ws: ws, streams: map[uint32]*stream{}, out: make(chan []byte, 256)}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go c.writer(ctx)
	c.out <- wisp.Continue(0, uint32(s.cfg.Buffer))
	c.reader(ctx)
	c.closeAll()
	ws.CloseNow()
}

// conn is one client WebSocket.
type conn struct {
	srv     *Server
	ws      *websocket.Conn
	mu      sync.Mutex
	streams map[uint32]*stream
	out     chan []byte // packets to the client, written by writer
}

type stream struct {
	id   uint32
	udp  bool
	in   chan []byte // client DATA waiting to be written upstream
	once sync.Once
	done chan struct{}
	sock net.Conn // set once dialed
}

func (c *conn) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt := <-c.out:
			if err := c.ws.Write(ctx, websocket.MessageBinary, pkt); err != nil {
				return
			}
		}
	}
}

func (c *conn) send(ctx context.Context, pkt []byte) {
	select {
	case c.out <- pkt:
	case <-ctx.Done():
	}
}

func (c *conn) reader(ctx context.Context) {
	for {
		_, msg, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		p, err := wisp.Decode(msg)
		if err != nil {
			continue
		}
		switch p.Type {
		case wisp.TypeConnect:
			c.connect(ctx, p)
		case wisp.TypeData:
			c.mu.Lock()
			st := c.streams[p.Stream]
			c.mu.Unlock()
			if st != nil {
				select {
				case st.in <- append([]byte(nil), p.Payload...):
				case <-st.done:
				case <-ctx.Done():
					return
				}
			}
		case wisp.TypeClose:
			c.mu.Lock()
			st := c.streams[p.Stream]
			delete(c.streams, p.Stream)
			c.mu.Unlock()
			if st != nil {
				st.stop()
			}
		}
	}
}

func (st *stream) stop() {
	st.once.Do(func() {
		close(st.done)
		if st.sock != nil {
			st.sock.Close()
		}
	})
}

func (c *conn) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range c.streams {
		st.stop()
		delete(c.streams, id)
	}
}

// finish ends a stream from the server side and tells the client why.
func (c *conn) finish(ctx context.Context, st *stream, reason uint8) {
	c.mu.Lock()
	_, open := c.streams[st.id]
	delete(c.streams, st.id)
	c.mu.Unlock()
	st.stop()
	if open {
		c.send(ctx, wisp.Close(st.id, reason))
	}
}

func (c *conn) connect(ctx context.Context, p wisp.Packet) {
	typ, port, host, err := wisp.ParseConnect(p.Payload)
	st := &stream{id: p.Stream, udp: typ == wisp.StreamUDP, done: make(chan struct{}),
		in: make(chan []byte, 2*c.srv.cfg.Buffer)}
	c.mu.Lock()
	_, dup := c.streams[p.Stream]
	full := len(c.streams) >= c.srv.cfg.MaxStreams
	if !dup && !full {
		c.streams[p.Stream] = st
	}
	c.mu.Unlock()
	switch {
	case dup:
		return
	case full:
		c.send(ctx, wisp.Close(p.Stream, wisp.CloseThrottled))
		return
	case err != nil || (typ != wisp.StreamTCP && typ != wisp.StreamUDP) || p.Stream == 0:
		c.finish(ctx, st, wisp.CloseInvalid)
		return
	case len(c.srv.cfg.Ports) > 0 && !c.srv.cfg.Ports[port]:
		c.finish(ctx, st, wisp.CloseBlocked)
		return
	}
	go c.run(ctx, st, host, port)
}

// run dials the destination and pumps data both ways until either side ends.
func (c *conn) run(ctx context.Context, st *stream, host string, port uint16) {
	addrs, reason := c.srv.resolve(ctx, host)
	if reason != 0 {
		c.finish(ctx, st, reason)
		return
	}
	network := "tcp"
	if st.udp {
		network = "udp"
	}
	// Try each permitted address in turn (e.g. IPv6 then IPv4).
	d := net.Dialer{Timeout: c.srv.cfg.DialTimeout}
	var sock net.Conn
	var err error
	for _, a := range addrs {
		if sock, err = d.DialContext(ctx, network, net.JoinHostPort(a.String(), strconv.Itoa(int(port)))); err == nil {
			break
		}
	}
	if err != nil {
		c.finish(ctx, st, dialReason(err))
		return
	}
	c.mu.Lock()
	st.sock = sock
	c.mu.Unlock()
	select {
	case <-st.done: // closed by the client while dialing
		sock.Close()
		return
	default:
	}

	// Client → destination, with CONTINUE credit refreshes (TCP only).
	go func() {
		buf := c.srv.cfg.Buffer
		consumed := 0
		for {
			select {
			case <-st.done:
				return
			case data := <-st.in:
				if _, err := sock.Write(data); err != nil {
					c.finish(ctx, st, wisp.CloseNetwork)
					return
				}
				if consumed++; !st.udp && consumed >= buf/2 {
					consumed = 0
					c.send(ctx, wisp.Continue(st.id, uint32(buf)))
				}
			}
		}
	}()

	// Destination → client.
	b := make([]byte, 32<<10)
	for {
		n, err := sock.Read(b)
		if n > 0 {
			c.send(ctx, wisp.Encode(wisp.TypeData, st.id, append([]byte(nil), b[:n]...)))
		}
		if err != nil {
			reason := uint8(wisp.CloseVoluntary)
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				reason = wisp.CloseNetwork
			}
			c.finish(ctx, st, reason)
			return
		}
	}
}

// resolve returns the permitted addresses for host, or a close reason. The
// checked addresses are dialed directly, so DNS rebinding cannot swap in a
// forbidden one afterwards.
func (s *Server) resolve(ctx context.Context, host string) ([]netip.Addr, uint8) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		rctx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
		defer cancel()
		ips, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, wisp.CloseUnreachable
		}
		addrs = ips
	}
	var ok []netip.Addr
	for _, a := range addrs {
		if a = a.Unmap(); s.cfg.AllowPrivate || public(a) {
			ok = append(ok, a)
		}
	}
	if len(ok) == 0 {
		return nil, wisp.CloseBlocked
	}
	return ok, 0
}

var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 could reach private IPv4
}

// public reports whether a is a globally routable unicast address.
func public(a netip.Addr) bool {
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func dialReason(err error) uint8 {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return wisp.CloseRefused
	case errors.As(err, &ne) && ne.Timeout():
		return wisp.CloseTimeout
	}
	return wisp.CloseUnreachable
}
