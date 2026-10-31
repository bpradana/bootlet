package netstack

import (
	"net"
	"strconv"
	"sync"
	"time"

	"bootlet/shared/wisp"
)

// Upstream carries the guest's TCP streams to the outside world. Open and
// Send never block: connecting happens in the background and a failure is
// reported as an EventClosed with Failed set.
type Upstream interface {
	Open(host string, port uint16) (id uint32)
	Send(id uint32, data []byte)
	Close(id uint32)
	// Poll returns the events that arrived since the last call.
	Poll() []Event
	// Pending reports whether Poll has events to return.
	Pending() bool
}

type EventKind int

const (
	EventData EventKind = iota + 1
	EventClosed
)

type Event struct {
	Kind   EventKind
	ID     uint32
	Data   []byte
	Failed bool // for EventClosed: the stream ended abnormally
}

// DirectUpstream opens real sockets from the host process (native runner).
type DirectUpstream struct {
	// Notify, if set, receives a value (without blocking) whenever events
	// arrive, so an idle runner can wake up.
	Notify chan struct{}

	mu     sync.Mutex
	next   uint32
	events []Event
	conns  map[uint32]*directConn
}

type directConn struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  [][]byte
	closed bool
	sock   net.Conn
}

func NewDirectUpstream() *DirectUpstream {
	return &DirectUpstream{Notify: make(chan struct{}, 1), conns: map[uint32]*directConn{}}
}

func (u *DirectUpstream) event(ev Event) {
	u.mu.Lock()
	u.events = append(u.events, ev)
	u.mu.Unlock()
	select {
	case u.Notify <- struct{}{}:
	default:
	}
}

func (u *DirectUpstream) Open(host string, port uint16) uint32 {
	u.mu.Lock()
	u.next++
	id := u.next
	c := &directConn{}
	c.cond = sync.NewCond(&c.mu)
	u.conns[id] = c
	u.mu.Unlock()
	go u.run(id, c, net.JoinHostPort(host, strconv.Itoa(int(port))))
	return id
}

func (u *DirectUpstream) run(id uint32, c *directConn, addr string) {
	sock, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		u.finish(id, true)
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		sock.Close()
		return
	}
	c.sock = sock
	c.mu.Unlock()

	go func() { // guest → socket
		for {
			c.mu.Lock()
			for len(c.queue) == 0 && !c.closed {
				c.cond.Wait()
			}
			if c.closed {
				c.mu.Unlock()
				return
			}
			data := c.queue[0]
			c.queue = c.queue[1:]
			c.mu.Unlock()
			if _, err := sock.Write(data); err != nil {
				sock.Close()
				return
			}
		}
	}()
	b := make([]byte, 32<<10)
	for {
		n, err := sock.Read(b)
		if n > 0 {
			u.event(Event{Kind: EventData, ID: id, Data: append([]byte(nil), b[:n]...)})
		}
		if err != nil {
			u.finish(id, false)
			return
		}
	}
}

// finish reports the end of a stream unless the guest side closed it first.
func (u *DirectUpstream) finish(id uint32, failed bool) {
	u.mu.Lock()
	c, open := u.conns[id]
	delete(u.conns, id)
	u.mu.Unlock()
	if !open {
		return
	}
	c.shut()
	u.event(Event{Kind: EventClosed, ID: id, Failed: failed})
}

func (c *directConn) shut() {
	c.mu.Lock()
	c.closed = true
	if c.sock != nil {
		c.sock.Close()
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (u *DirectUpstream) Send(id uint32, data []byte) {
	u.mu.Lock()
	c := u.conns[id]
	u.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.queue = append(c.queue, append([]byte(nil), data...))
	c.cond.Signal()
	c.mu.Unlock()
}

func (u *DirectUpstream) Close(id uint32) {
	u.mu.Lock()
	c := u.conns[id]
	delete(u.conns, id)
	u.mu.Unlock()
	if c != nil {
		c.shut()
	}
}

func (u *DirectUpstream) Poll() []Event {
	u.mu.Lock()
	defer u.mu.Unlock()
	ev := u.events
	u.events = nil
	return ev
}

func (u *DirectUpstream) Pending() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.events) > 0
}

// WispUpstream carries streams over a Wisp connection without doing any
// I/O itself: the host moves packets between Output/Input and a WebSocket.
// Before the relay's greeting arrives (or after the connection is lost), new
// streams fail at once, so the guest sees "connection refused" rather than
// a hang.
type WispUpstream struct {
	c          *wisp.Client
	open       map[uint32]bool
	events     []Event
	connecting bool
	failed     uint32
}

// Connecting must be called when a WebSocket to the relay is being opened:
// streams opened from now on wait for the relay's greeting instead of
// failing.
func (u *WispUpstream) Connecting() { u.connecting = true }

func NewWispUpstream() *WispUpstream {
	return &WispUpstream{c: wisp.NewClient(), open: map[uint32]bool{}}
}

func (u *WispUpstream) Open(host string, port uint16) uint32 {
	if !u.c.Ready() && !u.connecting {
		// Offline: fail without involving the Wisp client. These ids come
		// from the top half of the range, which Wisp stream ids never reach.
		u.failed++
		id := 1<<31 | u.failed
		u.events = append(u.events, Event{Kind: EventClosed, ID: id, Failed: true})
		return id
	}
	id := u.c.Connect(host, port)
	u.open[id] = true
	return id
}

func (u *WispUpstream) Send(id uint32, data []byte) { u.c.Send(id, data) }

func (u *WispUpstream) Close(id uint32) {
	if u.open[id] {
		delete(u.open, id)
		u.c.Close(id, wisp.CloseVoluntary)
	}
}

// Input processes one packet from the relay.
func (u *WispUpstream) Input(pkt []byte) {
	ev, ok := u.c.Receive(pkt)
	if !ok {
		return
	}
	switch ev.Type {
	case wisp.EventData:
		u.events = append(u.events, Event{Kind: EventData, ID: ev.Stream, Data: append([]byte(nil), ev.Data...)})
	case wisp.EventClosed:
		delete(u.open, ev.Stream)
		u.events = append(u.events, Event{Kind: EventClosed, ID: ev.Stream, Failed: ev.Reason != wisp.CloseVoluntary})
	}
}

// Output returns the packets to send to the relay.
func (u *WispUpstream) Output() [][]byte { return u.c.Outgoing() }

// Disconnected must be called when the WebSocket is lost: every open stream
// fails and the client starts over (waiting for the next server hello).
func (u *WispUpstream) Disconnected() {
	u.connecting = false
	for id := range u.open {
		u.events = append(u.events, Event{Kind: EventClosed, ID: id, Failed: true})
	}
	clear(u.open)
	u.c.Reset()
}

func (u *WispUpstream) Poll() []Event {
	ev := u.events
	u.events = nil
	return ev
}

func (u *WispUpstream) Pending() bool { return len(u.events) > 0 }
