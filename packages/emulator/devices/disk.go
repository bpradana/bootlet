package devices

import (
	"crypto/rand"
	"encoding/binary"
	"sort"
	"time"
)

// BlockSize is the granularity of Disk storage and dirty tracking.
const BlockSize = 4096

// Backing is persistent storage for a Disk's blocks. ReadBlocks fills dst
// (len(idx) blocks, in order) with the stored contents, and zeros for blocks
// that were never stored. It is called synchronously from the emulator.
type Backing interface {
	ReadBlocks(idx []uint64, dst []byte)
}

// Disk is a sparse disk image. Written blocks are tracked so the host can
// persist just the changes (to OPFS/IndexedDB in the browser, or a file).
//
// With a Backing, only blocks that are not yet persisted stay in memory:
// those written since the last TakeDirty, and batches taken but not yet
// confirmed with Saved. Everything else is read from the backing on demand,
// so memory use does not grow with the amount of data on the disk.
// Without one, every block lives in memory (blocks).
type Disk struct {
	size    uint64
	backing Backing
	blocks  map[uint64][]byte   // in-memory mode only: all stored blocks
	dirty   map[uint64][]byte   // written since the last TakeDirty
	pending []map[uint64][]byte // taken, not yet Saved; oldest first
	scratch []byte              // reused buffer for backing reads
}

func NewDisk(size uint64) *Disk {
	return &Disk{size: size &^ (BlockSize - 1), blocks: map[uint64][]byte{}, dirty: map[uint64][]byte{}}
}

// NewBackedDisk returns a disk whose persisted blocks live in b.
func NewBackedDisk(size uint64, b Backing) *Disk {
	d := NewDisk(size)
	d.backing, d.blocks = b, nil
	return d
}

func (d *Disk) Size() uint64 { return d.size }

// cached returns the in-memory copy of block idx: unsaved data first, then
// (without a backing) the stored blocks.
func (d *Disk) cached(idx uint64) []byte {
	if b := d.dirty[idx]; b != nil {
		return b
	}
	for i := len(d.pending) - 1; i >= 0; i-- {
		if b := d.pending[i][idx]; b != nil {
			return b
		}
	}
	return d.blocks[idx]
}

// ReadAt fills p from offset off.
func (d *Disk) ReadAt(p []byte, off uint64) {
	var missing []uint64 // blocks to fetch from the backing
	var missingAt []int  // ...and where they go in p
	for pos := 0; pos < len(p); {
		cur := off + uint64(pos)
		idx, o := cur/BlockSize, cur%BlockSize
		n := min(len(p)-pos, int(BlockSize-o))
		if b := d.cached(idx); b != nil {
			copy(p[pos:pos+n], b[o:])
		} else if d.backing != nil {
			missing = append(missing, idx)
			missingAt = append(missingAt, pos)
		} else {
			clear(p[pos : pos+n])
		}
		pos += n
	}
	if len(missing) == 0 {
		return
	}
	if need := len(missing) * BlockSize; cap(d.scratch) < need {
		d.scratch = make([]byte, need)
	}
	buf := d.scratch[:len(missing)*BlockSize]
	d.backing.ReadBlocks(missing, buf)
	for i, pos := range missingAt {
		o := int((off + uint64(pos)) % BlockSize)
		n := min(len(p)-pos, BlockSize-o)
		copy(p[pos:pos+n], buf[i*BlockSize+o:])
	}
}

// WriteAt stores p at offset off and marks the touched blocks dirty.
func (d *Disk) WriteAt(p []byte, off uint64) {
	for len(p) > 0 {
		idx, o := off/BlockSize, off%BlockSize
		n := min(uint64(len(p)), BlockSize-o)
		b := d.dirty[idx]
		if b == nil {
			b = make([]byte, BlockSize)
			if n < BlockSize { // partial write: start from the current contents
				d.ReadAt(b, idx*BlockSize)
			}
			d.dirty[idx] = b
			if d.blocks != nil {
				d.blocks[idx] = b
			}
		}
		copy(b[o:], p[:n])
		p, off = p[n:], off+n
	}
}

// Restore loads a previously persisted block into an in-memory disk without
// marking it dirty. data is kept, not copied.
func (d *Disk) Restore(idx uint64, data []byte) {
	if idx*BlockSize >= d.size || d.blocks == nil {
		return
	}
	d.blocks[idx] = data[:BlockSize:BlockSize]
}

// TakeDirty returns the indexes and contents of blocks written since the last
// call, in ascending order, and clears the dirty set. With a backing, the
// blocks stay in memory until Saved confirms they were persisted.
func (d *Disk) TakeDirty() (idx []uint64, data [][]byte) {
	for i := range d.dirty {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	for _, i := range idx {
		data = append(data, d.dirty[i])
	}
	if d.backing != nil && len(idx) > 0 {
		d.pending = append(d.pending, d.dirty)
	}
	d.dirty = map[uint64][]byte{}
	return idx, data
}

// Saved tells the disk that the oldest batch returned by TakeDirty is now in
// the backing, so its blocks can be dropped from memory.
func (d *Disk) Saved() {
	if len(d.pending) > 0 {
		d.pending[0] = nil
		d.pending = d.pending[1:]
	}
}

// Unsaved returns the number of blocks held in memory awaiting persistence.
func (d *Disk) Unsaved() int {
	n := len(d.dirty)
	for _, p := range d.pending {
		n += len(p)
	}
	return n
}

// Format writes an empty filesystem (4 KiB blocks, root directory and
// lost+found) to the disk. BusyBox has no mkfs, so a fresh disk is formatted
// here instead of in the guest. Every block group carries a copy of the
// superblock and group descriptors (no sparse_super); only metadata blocks
// are written, so the sparse disk stays small.
//
// Disks of 64 MiB and more get an 8 MiB journal (ext3 layout, mounted with
// the ext4 driver). The browser saves the disk in periodic batches, so after
// a reload or a closed tab the disk is as if the power had been cut; the
// kernel replays the journal on the next mount instead of finding a corrupt
// filesystem.
func (d *Disk) Format(label string) {
	const (
		bs        = BlockSize
		bpg       = 8 * bs // blocks per group: one bitmap block's worth
		inodeSize = 128
		firstIno  = 11
	)
	blocks := uint32(d.size / bs)
	groups := (blocks + bpg - 1) / bpg
	// One inode per 16 KiB, a whole number of inode-table blocks per group.
	ipg := min(uint32(8192), min(blocks, bpg)/4) &^ (bs/inodeSize - 1)
	itBlocks := ipg * inodeSize / bs
	overhead := 4 + itBlocks // superblock, GDT, two bitmaps, inode table
	if last := blocks - (groups-1)*bpg; groups > 1 && last < overhead+16 {
		groups-- // drop a runt last group
		blocks = groups * bpg
	}
	rootBlk, lfBlk := overhead, overhead+1
	now := uint32(time.Now().Unix())
	le := binary.LittleEndian

	// The journal (inode 8) follows the directories in group 0: its data
	// blocks, then the indirect blocks that map them.
	var journalBlocks uint32
	if blocks >= 16384 {
		journalBlocks = 2048
	}
	jStart := lfBlk + 1
	journalMeta := journalMap(journalBlocks)
	groupBlocks := func(g uint32) uint32 { return min(bpg, blocks-g*bpg) }
	usedIn := func(g uint32) uint32 {
		if g == 0 {
			return overhead + 2 + journalBlocks + journalMeta // directories, journal
		}
		return overhead
	}
	var freeBlocks uint32
	for g := uint32(0); g < groups; g++ {
		freeBlocks += groupBlocks(g) - usedIn(g)
	}

	sb := make([]byte, 1024)
	le.PutUint32(sb[0:], ipg*groups)
	le.PutUint32(sb[4:], blocks)
	le.PutUint32(sb[12:], freeBlocks)
	le.PutUint32(sb[16:], ipg*groups-firstIno)
	le.PutUint32(sb[20:], 0) // first data block
	le.PutUint32(sb[24:], 2) // log2(block size) - 10
	le.PutUint32(sb[28:], 2) // fragment size
	le.PutUint32(sb[32:], bpg)
	le.PutUint32(sb[36:], bpg)
	le.PutUint32(sb[40:], ipg)
	le.PutUint32(sb[48:], now)
	le.PutUint16(sb[54:], 0xffff) // max mount count: no forced fsck
	le.PutUint16(sb[56:], 0xef53)
	le.PutUint16(sb[58:], 1) // clean
	le.PutUint16(sb[60:], 1) // errors: continue
	le.PutUint32(sb[64:], now)
	le.PutUint32(sb[76:], 1) // revision 1 (dynamic)
	le.PutUint32(sb[84:], firstIno)
	le.PutUint16(sb[88:], inodeSize)
	le.PutUint32(sb[96:], 0x2) // incompat: filetype in directory entries
	rand.Read(sb[104:120])     // UUID
	copy(sb[120:136], label)
	if journalBlocks > 0 {
		le.PutUint32(sb[92:], 0x4) // compat: has_journal
		le.PutUint32(sb[224:], 8)  // journal inode
	}

	gdt := make([]byte, bs)
	for g := uint32(0); g < groups; g++ {
		base := g * bpg
		e := gdt[32*g:]
		le.PutUint32(e[0:], base+2) // block bitmap
		le.PutUint32(e[4:], base+3) // inode bitmap
		le.PutUint32(e[8:], base+4) // inode table
		le.PutUint16(e[12:], uint16(groupBlocks(g)-usedIn(g)))
		free := ipg
		if g == 0 {
			free -= firstIno
			le.PutUint16(e[16:], 2) // directories
		}
		le.PutUint16(e[14:], uint16(free))
	}

	for g := uint32(0); g < groups; g++ {
		base := uint64(g*bpg) * bs
		copySB := append([]byte(nil), sb...)
		le.PutUint16(copySB[90:], uint16(g)) // block group of this copy
		if g == 0 {
			d.WriteAt(copySB, 1024) // the primary sits 1 KiB into block 0
		} else {
			d.WriteAt(copySB, base)
		}
		d.WriteAt(gdt, base+bs)

		bm := make([]byte, bs)
		setBits(bm, 0, usedIn(g))
		setBits(bm, groupBlocks(g), bpg) // past the end of a short last group
		d.WriteAt(bm, base+2*bs)
		im := make([]byte, bs)
		if g == 0 {
			setBits(im, 0, firstIno)
		}
		setBits(im, ipg, 8*bs)
		d.WriteAt(im, base+3*bs)
	}

	// Inodes 2 (root) and 11 (lost+found), in group 0's inode table.
	dirInode := func(ino uint32, mode uint16, links uint16, blk uint32) {
		in := make([]byte, inodeSize)
		le.PutUint16(in[0:], mode)
		le.PutUint32(in[4:], bs)
		le.PutUint32(in[8:], now)
		le.PutUint32(in[12:], now)
		le.PutUint32(in[16:], now)
		le.PutUint16(in[26:], links)
		le.PutUint32(in[28:], bs/512)
		le.PutUint32(in[40:], blk)
		d.WriteAt(in, 4*bs+uint64(ino-1)*inodeSize)
	}
	dirInode(2, 0o40755, 3, rootBlk)
	dirInode(firstIno, 0o40700, 2, lfBlk)

	dirent := func(b []byte, ino uint32, recLen uint16, name string) {
		le.PutUint32(b[0:], ino)
		le.PutUint16(b[4:], recLen)
		b[6] = byte(len(name))
		b[7] = 2 // directory
		copy(b[8:], name)
	}
	root := make([]byte, bs)
	dirent(root[0:], 2, 12, ".")
	dirent(root[12:], 2, 12, "..")
	dirent(root[24:], firstIno, bs-24, "lost+found")
	d.WriteAt(root, uint64(rootBlk)*bs)
	lf := make([]byte, bs)
	dirent(lf[0:], firstIno, 12, ".")
	dirent(lf[12:], 2, bs-12, "..")
	d.WriteAt(lf, uint64(lfBlk)*bs)

	if journalBlocks > 0 {
		d.writeJournal(jStart, journalBlocks, sb[104:120], now)
	}
}

// journalMap returns how many indirect blocks map a journal of n blocks with
// classic block pointers (12 direct, one indirect, then double indirect).
func journalMap(n uint32) uint32 {
	const per = BlockSize / 4
	if n <= 12 {
		return 0
	}
	if n <= 12+per {
		return 1
	}
	return 2 + (n-12-per+per-1)/per
}

// writeJournal writes inode 8 and an empty (clean) JBD2 journal of n blocks
// starting at block start, followed by its indirect blocks.
func (d *Disk) writeJournal(start, n uint32, uuid []byte, now uint32) {
	const (
		bs        = BlockSize
		per       = bs / 4
		inodeSize = 128
	)
	le, be := binary.LittleEndian, binary.BigEndian
	meta := start + n // next free block for indirect blocks
	in := make([]byte, inodeSize)
	le.PutUint16(in[0:], 0o100600)
	le.PutUint32(in[4:], n*bs)
	le.PutUint32(in[8:], now)
	le.PutUint32(in[12:], now)
	le.PutUint32(in[16:], now)
	le.PutUint16(in[26:], 1)
	le.PutUint32(in[28:], (n+journalMap(n))*bs/512)
	for i := uint32(0); i < min(n, 12); i++ {
		le.PutUint32(in[40+4*i:], start+i)
	}
	// pointers fills one indirect block with consecutive journal blocks.
	pointers := func(at, first, count uint32) {
		b := make([]byte, bs)
		for i := uint32(0); i < count; i++ {
			le.PutUint32(b[4*i:], start+first+i)
		}
		d.WriteAt(b, uint64(at)*bs)
	}
	if n > 12 {
		ind := meta
		meta++
		le.PutUint32(in[40+4*12:], ind)
		pointers(ind, 12, min(n-12, per))
	}
	if n > 12+per {
		dind := meta
		meta++
		le.PutUint32(in[40+4*13:], dind)
		table := make([]byte, bs)
		for i, first := uint32(0), uint32(12+per); first < n; i, first = i+1, first+per {
			le.PutUint32(table[4*i:], meta)
			pointers(meta, first, min(n-first, per))
			meta++
		}
		d.WriteAt(table, uint64(dind)*bs)
	}
	d.WriteAt(in, 4*bs+7*inodeSize) // inode 8

	// JBD2 superblock (big-endian), version 2, nothing to replay (s_start 0).
	js := make([]byte, bs)
	be.PutUint32(js[0x00:], 0xc03b3998) // magic
	be.PutUint32(js[0x04:], 4)          // superblock v2
	be.PutUint32(js[0x0c:], bs)
	be.PutUint32(js[0x10:], n)
	be.PutUint32(js[0x14:], 1) // first log block
	be.PutUint32(js[0x18:], 1) // first transaction sequence
	copy(js[0x30:], uuid)
	be.PutUint32(js[0x40:], 1) // users
	copy(js[0x100:], uuid)
	d.WriteAt(js, uint64(start)*bs)
}

func setBits(b []byte, from, to uint32) {
	for i := from; i < to && i/8 < uint32(len(b)); i++ {
		b[i/8] |= 1 << (i % 8)
	}
}
