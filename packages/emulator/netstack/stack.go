// Package netstack is a user-mode network for the guest, in the spirit of
// QEMU's slirp: the guest sees an Ethernet LAN with a gateway and a DNS
// server, and its TCP connections are terminated here and carried over an
// Upstream (direct sockets natively, Wisp streams in the browser).
//
// The link between guest and stack is lossless: frames for the guest are
// queued until it has receive buffers, so TCP needs no retransmission.
package netstack

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// Fixed addresses of the guest LAN (10.0.2.0/24, as in QEMU user networking).
var (
	GuestIP    = [4]byte{10, 0, 2, 15}
	GatewayIP  = [4]byte{10, 0, 2, 2}
	DNSIP      = [4]byte{10, 0, 2, 3}
	GatewayMAC = [6]byte{0x52, 0x55, 0x0a, 0x00, 0x02, 0x02}
	GuestMAC   = [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
)

// GatewayHost is the upstream host name used for connections the guest makes
// to the gateway address: the machine running the emulator (or the relay).
const GatewayHost = "localhost"

const (
	etherIPv4 = 0x0800
	etherARP  = 0x0806

	protoICMP = 1
	protoTCP  = 6
	protoUDP  = 17
)

// Stack is the guest's network. It is not safe for concurrent use; the
// machine calls it from its run loop.
type Stack struct {
	up  Upstream
	out [][]byte // frames for the guest

	guestMAC [6]byte
	ipID     uint16

	// Fake DNS: every name gets an address from 198.18.0.0/15, and
	// connections to it are opened by name so the upstream resolves it.
	names    map[[4]byte]string
	addrs    map[string][4]byte
	nextFake uint32

	conns map[connKey]*tcpConn
	byID  map[uint32]*tcpConn
	iss   uint32
}

func New(up Upstream) *Stack {
	return &Stack{
		up:       up,
		guestMAC: GuestMAC,
		names:    map[[4]byte]string{},
		addrs:    map[string][4]byte{},
		nextFake: 198<<24 | 18<<16 | 1,
		conns:    map[connKey]*tcpConn{},
		byID:     map[uint32]*tcpConn{},
		iss:      0x1000,
	}
}

// Reset closes every connection, for when the guest reboots.
func (s *Stack) Reset() {
	for _, c := range s.conns {
		s.drop(c)
	}
	s.out = nil
}

// Frames returns and clears the frames waiting for the guest.
func (s *Stack) Frames() [][]byte {
	out := s.out
	s.out = nil
	return out
}

// Busy reports whether upstream events are waiting to be processed.
func (s *Stack) Busy() bool { return s.up.Pending() }

// Receive handles one Ethernet frame transmitted by the guest.
func (s *Stack) Receive(f []byte) {
	if len(f) < 14 {
		return
	}
	copy(s.guestMAC[:], f[6:12])
	switch binary.BigEndian.Uint16(f[12:]) {
	case etherARP:
		s.arp(f[14:])
	case etherIPv4:
		s.ipv4(f[14:])
	}
}

// Poll processes upstream events (data and closes) into guest frames.
func (s *Stack) Poll() {
	for _, ev := range s.up.Poll() {
		c := s.byID[ev.ID]
		if c == nil {
			continue
		}
		switch ev.Kind {
		case EventData:
			c.pending = append(c.pending, ev.Data...)
		case EventClosed:
			c.upClosed = true
			if ev.Failed {
				// Refused, unreachable, blocked, or a network error.
				s.reset(c)
				continue
			}
		}
		s.transmit(c)
	}
}

func (s *Stack) emit(dstMAC [6]byte, etherType uint16, payload []byte) {
	f := make([]byte, 14+len(payload))
	copy(f, dstMAC[:])
	copy(f[6:], GatewayMAC[:])
	binary.BigEndian.PutUint16(f[12:], etherType)
	copy(f[14:], payload)
	s.out = append(s.out, f)
}

func (s *Stack) arp(p []byte) {
	if len(p) < 28 || binary.BigEndian.Uint16(p[6:]) != 1 { // request
		return
	}
	var target [4]byte
	copy(target[:], p[24:28])
	if target != GatewayIP && target != DNSIP {
		return
	}
	r := make([]byte, 28)
	copy(r, p[:6])                       // htype, ptype, hlen, plen
	binary.BigEndian.PutUint16(r[6:], 2) // reply
	copy(r[8:], GatewayMAC[:])           // sender MAC
	copy(r[14:], target[:])              // sender IP
	copy(r[18:], p[8:14])                // target MAC
	copy(r[24:], p[14:18])               // target IP
	s.emit(s.guestMAC, etherARP, r)
}

func checksum(sum uint32, b []byte) uint32 {
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b))
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	return sum
}

func fold(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// sendIP wraps payload in an IPv4 header from src to the guest.
func (s *Stack) sendIP(src [4]byte, proto byte, payload []byte) {
	h := make([]byte, 20+len(payload))
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(len(h)))
	s.ipID++
	binary.BigEndian.PutUint16(h[4:], s.ipID)
	h[6] = 0x40 // don't fragment
	h[8] = 64
	h[9] = proto
	copy(h[12:], src[:])
	copy(h[16:], GuestIP[:])
	binary.BigEndian.PutUint16(h[10:], fold(checksum(0, h[:20])))
	copy(h[20:], payload)
	s.emit(s.guestMAC, etherIPv4, h)
}

// pseudo returns the checksum of the TCP/UDP pseudo header.
func pseudo(src, dst [4]byte, proto byte, length int) uint32 {
	sum := checksum(0, src[:])
	sum = checksum(sum, dst[:])
	return sum + uint32(proto) + uint32(length)
}

func (s *Stack) ipv4(p []byte) {
	if len(p) < 20 || p[0]>>4 != 4 {
		return
	}
	ihl := int(p[0]&0xf) * 4
	total := int(binary.BigEndian.Uint16(p[2:]))
	if ihl < 20 || total < ihl || total > len(p) {
		return
	}
	if binary.BigEndian.Uint16(p[6:])&0x3fff != 0 { // fragments are not supported
		return
	}
	var src, dst [4]byte
	copy(src[:], p[12:16])
	copy(dst[:], p[16:20])
	body := p[ihl:total]
	switch p[9] {
	case protoICMP:
		s.icmp(dst, body)
	case protoUDP:
		s.udp(dst, body)
	case protoTCP:
		s.tcp(src, dst, body)
	}
}

// icmp answers echo requests to the gateway and DNS addresses.
func (s *Stack) icmp(dst [4]byte, p []byte) {
	if len(p) < 8 || p[0] != 8 || (dst != GatewayIP && dst != DNSIP) {
		return
	}
	r := append([]byte(nil), p...)
	r[0] = 0 // echo reply
	r[2], r[3] = 0, 0
	binary.BigEndian.PutUint16(r[2:], fold(checksum(0, r)))
	s.sendIP(dst, protoICMP, r)
}

func (s *Stack) udp(dst [4]byte, p []byte) {
	if len(p) < 8 {
		return
	}
	srcPort := binary.BigEndian.Uint16(p)
	dstPort := binary.BigEndian.Uint16(p[2:])
	if dst != DNSIP || dstPort != 53 {
		return // only DNS is served; other UDP is dropped
	}
	reply := s.dns(p[8:])
	if reply == nil {
		return
	}
	u := make([]byte, 8+len(reply))
	binary.BigEndian.PutUint16(u, 53)
	binary.BigEndian.PutUint16(u[2:], srcPort)
	binary.BigEndian.PutUint16(u[4:], uint16(len(u)))
	copy(u[8:], reply)
	binary.BigEndian.PutUint16(u[6:], fold(checksum(pseudo(DNSIP, GuestIP, protoUDP, len(u)), u)))
	s.sendIP(DNSIP, protoUDP, u)
}

// dns answers a query: A records get a fake address that maps back to the
// name, every other type gets an empty answer.
func (s *Stack) dns(q []byte) []byte {
	if len(q) < 12 || q[2]&0x80 != 0 || binary.BigEndian.Uint16(q[4:]) != 1 {
		return nil
	}
	var labels []string
	i := 12
	for i < len(q) && q[i] != 0 {
		n := int(q[i])
		if n&0xc0 != 0 || i+1+n > len(q) {
			return nil
		}
		labels = append(labels, string(q[i+1:i+1+n]))
		i += 1 + n
	}
	if i+5 > len(q) {
		return nil
	}
	qend := i + 5
	qtype := binary.BigEndian.Uint16(q[i+1:])
	name := strings.ToLower(strings.Join(labels, "."))

	r := append([]byte(nil), q[:qend]...)
	r[2] = 0x80 | q[2]&0x01 // QR, opcode 0, RD copied
	r[3] = 0x80             // RA, NOERROR
	binary.BigEndian.PutUint16(r[6:], 0)
	binary.BigEndian.PutUint16(r[8:], 0)
	binary.BigEndian.PutUint16(r[10:], 0)
	if qtype != 1 || name == "" {
		return r
	}
	ip := s.lookup(name)
	binary.BigEndian.PutUint16(r[6:], 1)
	r = append(r, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
	return append(r, ip[:]...)
}

// lookup returns the address the guest should use for name.
func (s *Stack) lookup(name string) [4]byte {
	if ip, ok := parseIPv4(name); ok {
		return ip
	}
	if ip, ok := s.addrs[name]; ok {
		return ip
	}
	n := s.nextFake
	s.nextFake++
	ip := [4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	s.addrs[name] = ip
	s.names[ip] = name
	return ip
}

// hostFor returns the upstream host name for a destination address.
func (s *Stack) hostFor(ip [4]byte) string {
	if name, ok := s.names[ip]; ok {
		return name
	}
	if ip == GatewayIP {
		return GatewayHost
	}
	return strconv.Itoa(int(ip[0])) + "." + strconv.Itoa(int(ip[1])) + "." +
		strconv.Itoa(int(ip[2])) + "." + strconv.Itoa(int(ip[3]))
}

func parseIPv4(s string) ([4]byte, bool) {
	var ip [4]byte
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return ip, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return ip, false
		}
		ip[i] = byte(n)
	}
	return ip, true
}
