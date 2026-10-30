package devices

// VirtioNet is a virtio network device. Frames the guest transmits go to TX;
// frames queued with Deliver are written into the guest's receive buffers as
// they become available, so nothing is ever dropped.
type VirtioNet struct {
	*Virtio
	MAC [6]byte
	TX  func(frame []byte)
	rx  [][]byte
}

const (
	netQueueRX = 0
	netQueueTX = 1

	netFMAC = 1 << 5
	netHdr  = 12 // struct virtio_net_hdr_v1
)

func NewVirtioNet(mac [6]byte, ram []byte, ramBase uint64) *VirtioNet {
	n := &VirtioNet{MAC: mac}
	n.Virtio = newVirtio(n, 2, ram, ramBase)
	return n
}

func (n *VirtioNet) deviceID() uint32 { return 1 }
func (n *VirtioNet) features() uint64 { return virtioF1 | netFMAC }

func (n *VirtioNet) config(off uint64) byte {
	if off < 6 {
		return n.MAC[off]
	}
	return 0
}

// Reset drops queued frames along with the transport state.
func (n *VirtioNet) Reset() {
	n.rx = nil
	n.Virtio.Reset()
}

// Deliver queues frames for the guest and fills any available buffers.
func (n *VirtioNet) Deliver(frames ...[]byte) {
	n.rx = append(n.rx, frames...)
	n.flushRX()
}

// Pending reports whether frames are waiting for guest receive buffers.
func (n *VirtioNet) Pending() bool { return len(n.rx) > 0 }

func (n *VirtioNet) notify(q int) {
	switch q {
	case netQueueRX: // the driver added receive buffers
		n.flushRX()
	case netQueueTX:
		sent := false
		for {
			head, chain, ok := n.pop(netQueueTX)
			if !ok {
				break
			}
			var frame []byte
			for _, d := range chain {
				if d.flags&descFWrite == 0 {
					frame = append(frame, n.mem(d.addr, uint64(d.len))...)
				}
			}
			n.push(netQueueTX, head, 0)
			sent = true
			if len(frame) > netHdr && n.TX != nil {
				n.TX(frame[netHdr:])
			}
		}
		if sent {
			n.interrupt()
		}
	}
}

func (n *VirtioNet) flushRX() {
	if n.status&4 == 0 { // DRIVER_OK not set yet
		return
	}
	delivered := false
	for len(n.rx) > 0 {
		head, chain, ok := n.pop(netQueueRX)
		if !ok {
			break
		}
		pkt := make([]byte, netHdr+len(n.rx[0]))
		pkt[10] = 1 // num_buffers
		copy(pkt[netHdr:], n.rx[0])
		n.rx = n.rx[1:]
		var written uint32
		for _, d := range chain {
			if d.flags&descFWrite == 0 || len(pkt) == 0 {
				continue
			}
			buf := n.mem(d.addr, uint64(d.len))
			c := copy(buf, pkt)
			pkt = pkt[c:]
			written += uint32(c)
		}
		n.push(netQueueRX, head, written)
		delivered = true
	}
	if len(n.rx) == 0 {
		n.rx = nil
	}
	if delivered {
		n.interrupt()
	}
}
