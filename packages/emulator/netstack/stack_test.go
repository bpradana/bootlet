package netstack

import (
	"encoding/binary"
	"strconv"
	"testing"
)

// fakeUpstream records stack calls and lets tests inject events.
type fakeUpstream struct {
	opened []string
	sent   []byte
	closed []uint32
	events []Event
}

func (f *fakeUpstream) Open(host string, port uint16) uint32 {
	f.opened = append(f.opened, host+":"+strconv.Itoa(int(port)))
	return uint32(len(f.opened))
}
func (f *fakeUpstream) Send(_ uint32, d []byte) { f.sent = append(f.sent, d...) }
func (f *fakeUpstream) Close(id uint32)         { f.closed = append(f.closed, id) }
func (f *fakeUpstream) Poll() []Event           { e := f.events; f.events = nil; return e }
func (f *fakeUpstream) Pending() bool           { return len(f.events) > 0 }

func ether(etherType uint16, payload []byte) []byte {
	f := make([]byte, 14+len(payload))
	copy(f, GatewayMAC[:])
	copy(f[6:], GuestMAC[:])
	binary.BigEndian.PutUint16(f[12:], etherType)
	copy(f[14:], payload)
	return f
}

func ipFrame(dst [4]byte, proto byte, body []byte) []byte {
	h := make([]byte, 20+len(body))
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(len(h)))
	h[8] = 64
	h[9] = proto
	copy(h[12:], GuestIP[:])
	copy(h[16:], dst[:])
	copy(h[20:], body)
	return ether(etherIPv4, h)
}

func tcpFrame(dst [4]byte, dport uint16, seq, ack uint32, flags byte, data []byte) []byte {
	t := make([]byte, 20+len(data))
	binary.BigEndian.PutUint16(t, 40000)
	binary.BigEndian.PutUint16(t[2:], dport)
	binary.BigEndian.PutUint32(t[4:], seq)
	binary.BigEndian.PutUint32(t[8:], ack)
	t[12] = 5 << 4
	t[13] = flags
	binary.BigEndian.PutUint16(t[14:], 2000) // small window to exercise flow control
	copy(t[20:], data)
	return ipFrame(dst, protoTCP, t)
}

type seg struct {
	seq, ack uint32
	flags    byte
	data     []byte
}

// segments decodes the TCP segments among frames, checking checksums.
func segments(t *testing.T, frames [][]byte) []seg {
	var out []seg
	for _, f := range frames {
		ip := f[14:]
		if ip[9] != protoTCP {
			continue
		}
		if fold(checksum(0, ip[:20])) != 0 {
			t.Fatal("bad IP checksum")
		}
		var src [4]byte
		copy(src[:], ip[12:16])
		tcp := ip[20:]
		if fold(checksum(pseudo(src, GuestIP, protoTCP, len(tcp)), tcp)) != 0 {
			t.Fatal("bad TCP checksum")
		}
		off := int(tcp[12]>>4) * 4
		out = append(out, seg{binary.BigEndian.Uint32(tcp[4:]), binary.BigEndian.Uint32(tcp[8:]), tcp[13], tcp[off:]})
	}
	return out
}

func TestARPAndDNS(t *testing.T) {
	s := New(&fakeUpstream{})
	arp := make([]byte, 28)
	copy(arp, []byte{0, 1, 8, 0, 6, 4, 0, 1})
	copy(arp[8:], GuestMAC[:])
	copy(arp[14:], GuestIP[:])
	copy(arp[24:], GatewayIP[:])
	s.Receive(ether(etherARP, arp))
	f := s.Frames()
	if len(f) != 1 || binary.BigEndian.Uint16(f[0][14+6:]) != 2 || [6]byte(f[0][14+8:14+14]) != GatewayMAC {
		t.Fatalf("bad ARP reply %x", f)
	}

	query := func(qtype uint16) []byte {
		q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
		q = append(q, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, byte(qtype), 0, 1)
		u := make([]byte, 8+len(q))
		binary.BigEndian.PutUint16(u, 5353)
		binary.BigEndian.PutUint16(u[2:], 53)
		binary.BigEndian.PutUint16(u[4:], uint16(len(u)))
		copy(u[8:], q)
		s.Receive(ipFrame(DNSIP, protoUDP, u))
		f := s.Frames()
		if len(f) != 1 {
			t.Fatalf("no DNS reply")
		}
		return f[0][14+20+8:]
	}
	r := query(1)
	if binary.BigEndian.Uint16(r[6:]) != 1 || [4]byte(r[len(r)-4:]) != [4]byte{198, 18, 0, 1} {
		t.Fatalf("bad A answer %x", r)
	}
	if r := query(28); binary.BigEndian.Uint16(r[6:]) != 0 {
		t.Fatalf("AAAA should have no answers: %x", r)
	}
}

func TestTCP(t *testing.T) {
	up := &fakeUpstream{}
	s := New(up)
	fake := s.lookup("example.com")

	s.Receive(tcpFrame(fake, 80, 100, 0, tcpSYN, nil))
	if len(up.opened) != 1 || up.opened[0] != "example.com:80" {
		t.Fatalf("opened %v", up.opened)
	}
	synack := segments(t, s.Frames())
	if len(synack) != 1 || synack[0].flags != tcpSYN|tcpACK || synack[0].ack != 101 {
		t.Fatalf("bad SYN-ACK %+v", synack)
	}
	iss := synack[0].seq

	s.Receive(tcpFrame(fake, 80, 101, iss+1, tcpACK, []byte("GET /")))
	if string(up.sent) != "GET /" {
		t.Fatalf("upstream got %q", up.sent)
	}
	if a := segments(t, s.Frames()); len(a) != 1 || a[0].ack != 106 {
		t.Fatalf("data not acknowledged: %+v", a)
	}

	// 3000 bytes from upstream, but the guest window is only 2000.
	up.events = append(up.events, Event{Kind: EventData, ID: 1, Data: make([]byte, 3000)},
		Event{Kind: EventClosed, ID: 1})
	s.Poll()
	got := segments(t, s.Frames())
	if len(got) != 2 || len(got[0].data) != 1460 || len(got[1].data) != 540 {
		t.Fatalf("window not respected: %d segments", len(got))
	}
	s.Receive(tcpFrame(fake, 80, 106, iss+1+2000, tcpACK, nil))
	got = segments(t, s.Frames())
	if len(got) != 2 || len(got[0].data) != 1000 || got[1].flags&tcpFIN == 0 {
		t.Fatalf("remaining data + FIN not sent: %+v", got)
	}

	s.Receive(tcpFrame(fake, 80, 106, iss+1+3000+1, tcpACK|tcpFIN, nil))
	segments(t, s.Frames())
	if len(s.conns) != 0 {
		t.Fatal("connection not cleaned up after both FINs")
	}
}

func TestTCPRefused(t *testing.T) {
	up := &fakeUpstream{}
	s := New(up)
	s.Receive(tcpFrame([4]byte{1, 2, 3, 4}, 443, 1, 0, tcpSYN, nil))
	if up.opened[0] != "1.2.3.4:443" {
		t.Fatalf("opened %v", up.opened)
	}
	s.Frames()
	up.events = append(up.events, Event{Kind: EventClosed, ID: 1, Failed: true})
	s.Poll()
	if got := segments(t, s.Frames()); len(got) != 1 || got[0].flags&tcpRST == 0 {
		t.Fatalf("failed connect did not reset: %+v", got)
	}
}

func TestWispOffline(t *testing.T) {
	up := NewWispUpstream()
	s := New(up)
	s.Receive(tcpFrame([4]byte{1, 2, 3, 4}, 80, 1, 0, tcpSYN, nil))
	s.Frames()
	s.Poll()
	if got := segments(t, s.Frames()); len(got) != 1 || got[0].flags&tcpRST == 0 {
		t.Fatalf("connection without a relay should reset at once: %+v", got)
	}
	if out := up.Output(); len(out) != 0 {
		t.Fatalf("offline connection queued %d Wisp packets", len(out))
	}
}
