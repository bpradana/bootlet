// Package wisp implements the Wisp v1 protocol: TCP/UDP streams multiplexed
// over one WebSocket (https://github.com/MercuryWorkshop/wisp-protocol).
//
// The browser emulator is the client and apps/relay is the server. The
// client side here is sans-I/O: it turns stream operations into packets and
// packets into events, and the caller moves the bytes.
package wisp

import (
	"encoding/binary"
	"errors"
)

// Packet types.
const (
	TypeConnect  = 0x01
	TypeData     = 0x02
	TypeContinue = 0x03
	TypeClose    = 0x04
)

// Stream types.
const (
	StreamTCP = 0x01
	StreamUDP = 0x02
)

// Close reasons.
const (
	CloseUnknown     = 0x01
	CloseVoluntary   = 0x02
	CloseNetwork     = 0x03
	CloseInvalid     = 0x41
	CloseUnreachable = 0x42
	CloseTimeout     = 0x43
	CloseRefused     = 0x44
	CloseDataTimeout = 0x47
	CloseBlocked     = 0x48
	CloseThrottled   = 0x49
	CloseClientError = 0x81
)

// Packet is a decoded Wisp packet.
type Packet struct {
	Type    uint8
	Stream  uint32
	Payload []byte
}

func Encode(typ uint8, stream uint32, payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = typ
	binary.LittleEndian.PutUint32(b[1:], stream)
	copy(b[5:], payload)
	return b
}

func Decode(b []byte) (Packet, error) {
	if len(b) < 5 {
		return Packet{}, errors.New("wisp: short packet")
	}
	return Packet{Type: b[0], Stream: binary.LittleEndian.Uint32(b[1:]), Payload: b[5:]}, nil
}

func Connect(stream uint32, typ uint8, port uint16, host string) []byte {
	p := make([]byte, 3+len(host))
	p[0] = typ
	binary.LittleEndian.PutUint16(p[1:], port)
	copy(p[3:], host)
	return Encode(TypeConnect, stream, p)
}

// ParseConnect decodes a CONNECT payload.
func ParseConnect(p []byte) (typ uint8, port uint16, host string, err error) {
	if len(p) < 4 {
		return 0, 0, "", errors.New("wisp: short CONNECT")
	}
	return p[0], binary.LittleEndian.Uint16(p[1:]), string(p[3:]), nil
}

func Continue(stream uint32, buffer uint32) []byte {
	return Encode(TypeContinue, stream, binary.LittleEndian.AppendUint32(nil, buffer))
}

func Close(stream uint32, reason uint8) []byte {
	return Encode(TypeClose, stream, []byte{reason})
}

// EventType says what happened to a client stream.
type EventType int

const (
	EventData EventType = iota + 1
	EventClosed
)

// Event is something the server told the client about one of its streams.
type Event struct {
	Type   EventType
	Stream uint32
	Data   []byte
	Reason uint8
}

type clientStream struct {
	credit  int      // DATA packets the server can still buffer
	pending [][]byte // DATA payloads waiting for credit
}

// Client is the client end of a Wisp connection, without any I/O. Packets
// to send accumulate in Outgoing; packets received go to Receive.
type Client struct {
	ready   bool // the server's initial CONTINUE arrived
	initial int  // per-stream buffer size from that CONTINUE
	next    uint32
	streams map[uint32]*clientStream
	early   [][]byte // packets queued before ready
	out     [][]byte
}

func NewClient() *Client {
	return &Client{streams: map[uint32]*clientStream{}}
}

func (c *Client) send(pkt []byte) {
	if c.ready {
		c.out = append(c.out, pkt)
	} else {
		c.early = append(c.early, pkt)
	}
}

// Connect opens a TCP stream to host:port and returns its id.
func (c *Client) Connect(host string, port uint16) uint32 {
	c.next++
	id := c.next
	c.streams[id] = &clientStream{credit: c.initial}
	c.send(Connect(id, StreamTCP, port, host))
	return id
}

// Send queues data on a stream, respecting the server's buffer credit.
func (c *Client) Send(id uint32, data []byte) {
	s := c.streams[id]
	if s == nil {
		return
	}
	payload := append([]byte(nil), data...)
	if !c.ready || s.credit <= 0 || len(s.pending) > 0 {
		s.pending = append(s.pending, payload)
		return
	}
	s.credit--
	c.out = append(c.out, Encode(TypeData, id, payload))
}

// Close closes a stream from the client side.
func (c *Client) Close(id uint32, reason uint8) {
	if _, ok := c.streams[id]; !ok {
		return
	}
	delete(c.streams, id)
	c.send(Close(id, reason))
}

// Receive processes one packet from the server.
func (c *Client) Receive(b []byte) (Event, bool) {
	p, err := Decode(b)
	if err != nil {
		return Event{}, false
	}
	switch p.Type {
	case TypeContinue:
		if len(p.Payload) < 4 {
			return Event{}, false
		}
		credit := int(binary.LittleEndian.Uint32(p.Payload))
		if p.Stream == 0 {
			if !c.ready {
				c.ready, c.initial = true, credit
				for _, s := range c.streams {
					s.credit = credit
				}
				c.out = append(c.out, c.early...)
				c.early = nil
				for id := range c.streams {
					c.flush(id)
				}
			}
			return Event{}, false
		}
		if s := c.streams[p.Stream]; s != nil {
			s.credit = credit
			c.flush(p.Stream)
		}
	case TypeData:
		if _, ok := c.streams[p.Stream]; ok {
			return Event{Type: EventData, Stream: p.Stream, Data: p.Payload}, true
		}
	case TypeClose:
		if _, ok := c.streams[p.Stream]; ok {
			delete(c.streams, p.Stream)
			reason := uint8(CloseUnknown)
			if len(p.Payload) > 0 {
				reason = p.Payload[0]
			}
			return Event{Type: EventClosed, Stream: p.Stream, Reason: reason}, true
		}
	}
	return Event{}, false
}

func (c *Client) flush(id uint32) {
	s := c.streams[id]
	for len(s.pending) > 0 && s.credit > 0 {
		s.credit--
		c.out = append(c.out, Encode(TypeData, id, s.pending[0]))
		s.pending = s.pending[1:]
	}
}

// Outgoing returns and clears the packets waiting to be sent to the server.
func (c *Client) Outgoing() [][]byte {
	out := c.out
	c.out = nil
	return out
}

// Reset forgets every stream, for when the WebSocket is lost. The caller
// must treat all open streams as closed.
func (c *Client) Reset() {
	*c = Client{next: c.next, streams: map[uint32]*clientStream{}}
}
