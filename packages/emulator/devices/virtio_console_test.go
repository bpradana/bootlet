package devices

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// testDriver plays the guest side of a virtio-mmio device: each queue has
// its descriptor table, available and used rings in a fake RAM.
type testDriver struct {
	v    *Virtio
	ram  []byte
	next []uint16 // per queue: next descriptor / avail slot
	seen []uint16 // per queue: used entries consumed
}

const testRAMBase = 0x8000_0000

func newTestDriver(v *Virtio, ram []byte, queues int) *testDriver {
	d := &testDriver{v: v, ram: ram, next: make([]uint16, queues), seen: make([]uint16, queues)}
	for q := 0; q < queues; q++ {
		base := uint64(testRAMBase + q*0x1000)
		v.Write(0x030, 4, uint64(q))
		v.Write(0x038, 4, 16)
		v.Write(0x080, 4, base)
		v.Write(0x090, 4, base+0x100)
		v.Write(0x0a0, 4, base+0x200)
		v.Write(0x044, 4, 1)
	}
	v.Write(0x070, 4, 0xf) // DRIVER_OK
	return d
}

// add makes a single-descriptor buffer available on queue q and kicks it.
func (d *testDriver) add(q int, addr uint64, data []byte, writable bool, size uint32) {
	le := binary.LittleEndian
	base := testRAMBase + q*0x1000 - testRAMBase
	i := d.next[q]
	desc := d.ram[base+16*int(i%16):]
	flags := uint16(0)
	if writable {
		flags = descFWrite
	} else {
		copy(d.ram[addr-testRAMBase:], data)
		size = uint32(len(data))
	}
	le.PutUint64(desc, addr)
	le.PutUint32(desc[8:], size)
	le.PutUint16(desc[12:], flags)
	le.PutUint16(d.ram[base+0x100+4+2*int(i%16):], i%16)
	d.next[q]++
	le.PutUint16(d.ram[base+0x100+2:], d.next[q])
	d.v.Write(0x050, 4, uint64(q))
}

// used returns the buffers the device returned on queue q since the last call.
func (d *testDriver) used(q int) (lens []uint32) {
	le := binary.LittleEndian
	base := testRAMBase + q*0x1000 - testRAMBase
	for d.seen[q] != le.Uint16(d.ram[base+0x200+2:]) {
		e := d.ram[base+0x200+4+8*int(d.seen[q]%16):]
		lens = append(lens, le.Uint32(e[4:]))
		d.seen[q]++
	}
	return lens
}

func control(id uint32, event, value uint16) []byte {
	m := make([]byte, 8)
	binary.LittleEndian.PutUint32(m, id)
	binary.LittleEndian.PutUint16(m[4:], event)
	binary.LittleEndian.PutUint16(m[6:], value)
	return m
}

func TestVirtioConsole(t *testing.T) {
	ram := make([]byte, 0x10000)
	c := NewVirtioConsole("bootlet.agent", ram, testRAMBase)
	d := newTestDriver(c.Virtio, ram, 4)
	buf := func(n int) uint64 { return uint64(testRAMBase + 0x8000 + n*0x100) }
	read := func(n int, l uint32) []byte { return ram[buf(n)-testRAMBase:][:l] }

	// Handshake: DEVICE_READY → DEVICE_ADD(0); PORT_READY(0) → PORT_NAME, PORT_OPEN.
	for i := 0; i < 3; i++ {
		d.add(conQueueCtrlRX, buf(i), nil, true, 0x100)
	}
	d.add(conQueueCtrlTX, buf(10), control(0xffffffff, conDeviceReady, 1), false, 0)
	d.add(conQueueCtrlTX, buf(11), control(0, conPortReady, 1), false, 0)
	got := d.used(conQueueCtrlRX)
	if len(got) != 3 {
		t.Fatalf("control messages: got %d, want 3", len(got))
	}
	if !bytes.Equal(read(0, got[0]), control(0, conDeviceAdd, 1)) {
		t.Errorf("first control message % x, want DEVICE_ADD", read(0, got[0]))
	}
	if want := append(control(0, conPortName, 1), "bootlet.agent"...); !bytes.Equal(read(1, got[1]), want) {
		t.Errorf("PORT_NAME % x", read(1, got[1]))
	}
	if !bytes.Equal(read(2, got[2]), control(0, conPortOpen, 1)) {
		t.Errorf("PORT_OPEN % x", read(2, got[2]))
	}
	d.add(conQueueCtrlTX, buf(12), control(0, conPortOpen, 1), false, 0)
	if !c.Opened {
		t.Error("guest opening the port not recorded")
	}

	// Data queued before buffers exist is split across small buffers.
	c.Deliver([]byte("hello, guest"))
	d.add(conQueueRX, buf(20), nil, true, 5)
	d.add(conQueueRX, buf(21), nil, true, 5)
	d.add(conQueueRX, buf(22), nil, true, 5)
	got = d.used(conQueueRX)
	var rx []byte
	for i, l := range got {
		rx = append(rx, read(20+i, l)...)
	}
	if string(rx) != "hello, guest" {
		t.Errorf("guest received %q", rx)
	}

	d.add(conQueueTX, buf(30), []byte("hi "), false, 0)
	d.add(conQueueTX, buf(31), []byte("host"), false, 0)
	if out := c.Output(); string(out) != "hi host" {
		t.Errorf("host received %q", out)
	}
	if c.Output() != nil {
		t.Error("Output did not clear")
	}
}
