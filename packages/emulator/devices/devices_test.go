package devices

import "testing"

func TestPLICClaimComplete(t *testing.T) {
	p := NewPLIC()
	var line [2]bool
	p.Line = func(ctx int, level bool) { line[ctx] = level }
	p.Write(10*4, 4, 1)       // priority of source 10
	p.Write(0x2080, 4, 1<<10) // enable source 10 for context 1 (S-mode)
	p.Write(0x201000, 4, 0)   // threshold 0
	p.SetLevel(10, true)
	if !line[1] || line[0] {
		t.Fatalf("lines = %v, want only S-mode raised", line)
	}
	if v, _ := p.Read(0x201004, 4); v != 10 {
		t.Fatalf("claim = %d, want 10", v)
	}
	if line[1] {
		t.Fatal("line still raised while the source is claimed")
	}
	p.Write(0x201004, 4, 10) // complete with the level still high
	if !line[1] {
		t.Fatal("level-triggered source not re-raised after complete")
	}
	p.SetLevel(10, false)
	if line[1] {
		t.Fatal("line raised with no pending source")
	}
}

func TestUARTInterrupts(t *testing.T) {
	u := NewUART()
	var irq bool
	u.IRQ = func(level bool) { irq = level }

	u.Write8(1, ierRDI)
	u.Input([]byte("hi"))
	if !irq {
		t.Fatal("no interrupt for received data")
	}
	if v, _ := u.Read(2, 1); v&0x0f != 0x04 {
		t.Fatalf("IIR = %#x, want RDA", v)
	}
	if v, _ := u.Read(5, 1); v&lsrDR == 0 {
		t.Fatal("LSR.DR not set")
	}
	u.Read(0, 1)
	u.Read(0, 1)
	if irq {
		t.Fatal("interrupt still raised after draining input")
	}

	// THRE: enabling it raises an interrupt that reading IIR clears.
	u.Write8(1, ierTHRI)
	if !irq {
		t.Fatal("no THRE interrupt after enabling it")
	}
	if v, _ := u.Read(2, 1); v&0x0f != 0x02 {
		t.Fatalf("IIR = %#x, want THRE", v)
	}
	if irq {
		t.Fatal("THRE interrupt not cleared by reading IIR")
	}
	u.Write8(0, 'x')
	if !irq || string(u.Output()) != "x" {
		t.Fatal("transmit did not produce output and a THRE interrupt")
	}
}

func TestDiskDirtyTracking(t *testing.T) {
	d := NewDisk(1 << 20)
	d.Format("test")
	idx, data := d.TakeDirty()
	if len(idx) == 0 || len(idx) != len(data) {
		t.Fatalf("format left %d dirty blocks", len(idx))
	}
	if sb := data[0][1024+56:]; sb[0] != 0x53 || sb[1] != 0xef {
		t.Fatal("no ext2 magic in superblock")
	}
	if idx, _ := d.TakeDirty(); len(idx) != 0 {
		t.Fatal("dirty set not cleared")
	}
	d.WriteAt([]byte("hello"), BlockSize*7+4094)
	idx, _ = d.TakeDirty()
	if len(idx) != 2 || idx[0] != 7 || idx[1] != 8 {
		t.Fatalf("dirty blocks = %v, want [7 8]", idx)
	}
	buf := make([]byte, 5)
	d.ReadAt(buf, BlockSize*7+4094)
	if string(buf) != "hello" {
		t.Fatalf("read back %q", buf)
	}
}

func TestFormatMultiGroup(t *testing.T) {
	d := NewDisk(1 << 30)
	d.Format("big")
	sb := make([]byte, 1024)
	d.ReadAt(sb, 1024)
	le := func(off int) uint32 {
		return uint32(sb[off]) | uint32(sb[off+1])<<8 | uint32(sb[off+2])<<16 | uint32(sb[off+3])<<24
	}
	if le(4) != 262144 || le(32) != 32768 {
		t.Fatalf("blocks=%d per group=%d", le(4), le(32))
	}
	// Group 7's backup superblock must exist and name its group.
	backup := make([]byte, 1024)
	d.ReadAt(backup, 7*32768*BlockSize)
	if backup[56] != 0x53 || backup[57] != 0xef || backup[90] != 7 {
		t.Fatal("missing or wrong backup superblock in group 7")
	}
	idx, _ := d.TakeDirty()
	if len(idx) > 64 {
		t.Fatalf("format touched %d blocks; the disk should stay sparse", len(idx))
	}
}

// memBacking is a Backing that stores blocks in a map, counting reads.
type memBacking struct {
	blocks map[uint64][]byte
	reads  int
}

func (m *memBacking) ReadBlocks(idx []uint64, dst []byte) {
	m.reads++
	for i, b := range idx {
		out := dst[i*BlockSize : (i+1)*BlockSize]
		if data, ok := m.blocks[b]; ok {
			copy(out, data)
		} else {
			clear(out)
		}
	}
}

// save persists one batch the way the browser worker does.
func (m *memBacking) save(d *Disk) {
	idx, data := d.TakeDirty()
	for i, b := range idx {
		m.blocks[b] = append([]byte(nil), data[i]...)
	}
	d.Saved()
}

func TestBackedDisk(t *testing.T) {
	back := &memBacking{blocks: map[uint64][]byte{}}
	d := NewBackedDisk(1<<20, back)
	read := func(off uint64, n int) string {
		b := make([]byte, n)
		d.ReadAt(b, off)
		return string(b)
	}

	d.WriteAt([]byte("hello"), 4096*3+10)
	if got := read(4096*3+10, 5); got != "hello" {
		t.Fatalf("dirty read = %q", got)
	}
	idx, data := d.TakeDirty()
	if len(idx) != 1 || d.Unsaved() != 1 {
		t.Fatalf("taken %v, unsaved %d", idx, d.Unsaved())
	}
	if got := read(4096*3+10, 5); got != "hello" {
		t.Fatalf("pending read = %q", got)
	}
	back.blocks[idx[0]] = append([]byte(nil), data[0]...)
	d.Saved()
	if d.Unsaved() != 0 {
		t.Fatal("saved batch still held in memory")
	}
	if got := read(4096*3+10, 5); got != "hello" || back.reads == 0 {
		t.Fatalf("backing read = %q (reads %d)", got, back.reads)
	}
	if got := read(4096*7, 4); got != "\x00\x00\x00\x00" {
		t.Fatalf("never-stored block read %q", got)
	}

	// A partial write keeps the rest of the stored block, and a read
	// spanning cached and stored blocks comes back whole.
	d.WriteAt([]byte("HE"), 4096*3+10)
	if got := read(4096*3+10, 5); got != "HEllo" {
		t.Fatalf("partial write merged to %q", got)
	}
	d.WriteAt([]byte("next"), 4096*4)
	back.save(d)
	d.WriteAt([]byte("X"), 4096*4)
	if got := read(4096*3+10, 4096); got[:5] != "HEllo" || got[4086:4090] != "Xext" {
		t.Fatalf("spanning read = %q...%q", got[:5], got[4086:4090])
	}
}

func TestBackedDiskFormat(t *testing.T) {
	back := &memBacking{blocks: map[uint64][]byte{}}
	d := NewBackedDisk(1<<30, back)
	d.Format("t")
	back.save(d)
	sb := make([]byte, 1024)
	d.ReadAt(sb, 1024)
	if sb[56] != 0x53 || sb[57] != 0xef {
		t.Fatal("superblock not readable through the backing")
	}
}
