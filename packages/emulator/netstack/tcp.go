package netstack

import "encoding/binary"

const (
	tcpFIN = 0x01
	tcpSYN = 0x02
	tcpRST = 0x04
	tcpPSH = 0x08
	tcpACK = 0x10

	mss    = 1460
	window = 65535 // we never ask for window scaling, so this is the maximum
)

type connKey struct {
	rIP          [4]byte
	rPort, gPort uint16
}

// tcpConn is one guest TCP connection, terminated here. Sequence numbers:
// snd* are ours (stack → guest), rcvNxt is the guest's.
type tcpConn struct {
	key    connKey
	id     uint32 // upstream stream
	sndUna uint32 // oldest byte the guest has not acknowledged
	sndNxt uint32
	rcvNxt uint32
	wnd    uint32 // guest receive window
	mss    int

	pending  []byte // upstream data not yet sent to the guest
	upClosed bool   // upstream finished: send FIN once pending drains
	finSent  bool
	guestFin bool
}

func (s *Stack) tcp(src, dst [4]byte, p []byte) {
	if len(p) < 20 {
		return
	}
	be := binary.BigEndian
	gPort, rPort := be.Uint16(p), be.Uint16(p[2:])
	seq, ack := be.Uint32(p[4:]), be.Uint32(p[8:])
	off := int(p[12]>>4) * 4
	flags := p[13]
	wnd := uint32(be.Uint16(p[14:]))
	if off < 20 || off > len(p) {
		return
	}
	data := p[off:]
	key := connKey{dst, rPort, gPort}
	c := s.conns[key]

	if flags&tcpRST != 0 {
		if c != nil {
			s.drop(c)
		}
		return
	}
	if c == nil {
		if flags&tcpSYN == 0 || flags&tcpACK != 0 {
			if flags&tcpACK != 0 { // stale segment for a connection we forgot
				s.segment(key, ack, 0, tcpRST, nil)
			}
			return
		}
		s.open(key, seq, wnd, p[20:off])
		return
	}
	if flags&tcpSYN != 0 { // retransmitted SYN: repeat our SYN-ACK
		s.segment(key, c.sndUna, c.rcvNxt, tcpSYN|tcpACK, synOptions())
		return
	}
	if flags&tcpACK != 0 && int32(ack-c.sndUna) > 0 && int32(c.sndNxt-ack) >= 0 {
		c.sndUna = ack
	}
	c.wnd = wnd

	if len(data) > 0 || flags&tcpFIN != 0 {
		if seq != c.rcvNxt {
			// Out of order or a retransmission: re-acknowledge what we have.
			s.ack(c)
			return
		}
		if len(data) > 0 {
			if c.guestFin {
				return
			}
			s.up.Send(c.id, data)
			c.rcvNxt += uint32(len(data))
		}
		if flags&tcpFIN != 0 && !c.guestFin {
			// Wisp has no half-close, so the upstream stays open until it
			// closes itself or the guest resets.
			c.guestFin = true
			c.rcvNxt++
		}
		s.ack(c)
	}
	s.transmit(c)
}

func synOptions() []byte {
	o := []byte{2, 4, 0, 0} // MSS
	binary.BigEndian.PutUint16(o[2:], mss)
	return o
}

// open accepts a new connection optimistically: the guest gets its SYN-ACK
// right away and a failed upstream connect becomes a reset.
func (s *Stack) open(key connKey, seq, wnd uint32, opts []byte) {
	c := &tcpConn{key: key, rcvNxt: seq + 1, wnd: wnd, mss: mss}
	for i := 0; i < len(opts) && opts[i] != 0; { // 0 = end of options
		if opts[i] == 1 { // no-op
			i++
			continue
		}
		if i+1 >= len(opts) || opts[i+1] < 2 {
			break // malformed
		}
		if opts[i] == 2 && opts[i+1] == 4 && i+4 <= len(opts) {
			c.mss = min(mss, int(binary.BigEndian.Uint16(opts[i+2:])))
		}
		i += int(opts[i+1])
	}
	s.iss += 0x10000
	c.sndUna, c.sndNxt = s.iss, s.iss+1
	c.id = s.up.Open(s.hostFor(key.rIP), key.rPort)
	s.conns[key] = c
	s.byID[c.id] = c
	s.segment(key, s.iss, c.rcvNxt, tcpSYN|tcpACK, synOptions())
}

// transmit sends pending upstream data within the guest's window, then a
// FIN once the upstream has closed and everything was sent.
func (s *Stack) transmit(c *tcpConn) {
	if s.conns[c.key] != c {
		return
	}
	for len(c.pending) > 0 {
		inflight := c.sndNxt - c.sndUna
		if inflight >= c.wnd {
			break
		}
		n := min(len(c.pending), c.mss, int(c.wnd-inflight))
		s.segment(c.key, c.sndNxt, c.rcvNxt, tcpACK|tcpPSH, c.pending[:n])
		c.sndNxt += uint32(n)
		c.pending = c.pending[n:]
	}
	if len(c.pending) == 0 {
		c.pending = nil
		if c.upClosed && !c.finSent {
			s.segment(c.key, c.sndNxt, c.rcvNxt, tcpFIN|tcpACK, nil)
			c.sndNxt++
			c.finSent = true
		}
	}
	if c.finSent && c.guestFin && c.sndUna == c.sndNxt {
		s.drop(c)
	}
}

func (s *Stack) ack(c *tcpConn) {
	s.segment(c.key, c.sndNxt, c.rcvNxt, tcpACK, nil)
}

// reset aborts a connection towards the guest.
func (s *Stack) reset(c *tcpConn) {
	s.segment(c.key, c.sndNxt, c.rcvNxt, tcpRST|tcpACK, nil)
	s.drop(c)
}

func (s *Stack) drop(c *tcpConn) {
	if s.conns[c.key] == c {
		delete(s.conns, c.key)
	}
	delete(s.byID, c.id)
	if !c.upClosed {
		s.up.Close(c.id)
		c.upClosed = true
	}
}

// segment sends a TCP segment from the remote end of key to the guest.
func (s *Stack) segment(key connKey, seq, ack uint32, flags byte, data []byte) {
	opts := []byte(nil)
	if flags&tcpSYN != 0 {
		opts, data = data, nil
	}
	h := make([]byte, 20+len(opts)+len(data))
	be := binary.BigEndian
	be.PutUint16(h, key.rPort)
	be.PutUint16(h[2:], key.gPort)
	be.PutUint32(h[4:], seq)
	be.PutUint32(h[8:], ack)
	h[12] = byte((20 + len(opts)) / 4 << 4)
	h[13] = flags
	be.PutUint16(h[14:], window)
	copy(h[20:], opts)
	copy(h[20+len(opts):], data)
	be.PutUint16(h[16:], fold(checksum(pseudo(key.rIP, GuestIP, protoTCP, len(h)), h)))
	s.sendIP(key.rIP, protoTCP, h)
}
