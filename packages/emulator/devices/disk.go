package devices

import (
	"crypto/rand"
	"encoding/binary"
	"sort"
	"time"
)

// BlockSize is the granularity of Disk storage and dirty tracking.
const BlockSize = 4096

// Disk is a sparse in-memory disk image. Only blocks that were written hold
// memory; the rest read as zeros. Written blocks are tracked so the host can
// persist just the changes (to OPFS/IndexedDB in the browser, or a file).
type Disk struct {
	size   uint64
	blocks map[uint64][]byte
	dirty  map[uint64]bool
}

func NewDisk(size uint64) *Disk {
	return &Disk{size: size &^ (BlockSize - 1), blocks: map[uint64][]byte{}, dirty: map[uint64]bool{}}
}

func (d *Disk) Size() uint64 { return d.size }

// ReadAt fills p from offset off.
func (d *Disk) ReadAt(p []byte, off uint64) {
	for len(p) > 0 {
		idx, o := off/BlockSize, off%BlockSize
		n := min(uint64(len(p)), BlockSize-o)
		if b := d.blocks[idx]; b != nil {
			copy(p[:n], b[o:])
		} else {
			clear(p[:n])
		}
		p, off = p[n:], off+n
	}
}

// WriteAt stores p at offset off and marks the touched blocks dirty.
func (d *Disk) WriteAt(p []byte, off uint64) {
	for len(p) > 0 {
		idx, o := off/BlockSize, off%BlockSize
		n := min(uint64(len(p)), BlockSize-o)
		b := d.blocks[idx]
		if b == nil {
			b = make([]byte, BlockSize)
			d.blocks[idx] = b
		}
		copy(b[o:], p[:n])
		d.dirty[idx] = true
		p, off = p[n:], off+n
	}
}

// Restore loads a previously persisted block without marking it dirty.
func (d *Disk) Restore(idx uint64, data []byte) {
	if idx*BlockSize >= d.size {
		return
	}
	b := make([]byte, BlockSize)
	copy(b, data)
	d.blocks[idx] = b
}

// Empty reports whether nothing was ever written or restored.
func (d *Disk) Empty() bool { return len(d.blocks) == 0 }

// TakeDirty returns the indexes and contents of blocks written since the last
// call, in ascending order, and clears the dirty set.
func (d *Disk) TakeDirty() (idx []uint64, data [][]byte) {
	for i := range d.dirty {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	for _, i := range idx {
		data = append(data, append([]byte(nil), d.blocks[i]...))
	}
	clear(d.dirty)
	return idx, data
}

// FormatExt2 writes an empty ext2 filesystem (4 KiB blocks, root directory
// and lost+found) to the disk. BusyBox has no mkfs.ext2, so a fresh disk is
// formatted here instead of in the guest. Every block group carries a copy
// of the superblock and group descriptors (no sparse_super); only metadata
// blocks are written, so the sparse disk stays small.
func (d *Disk) FormatExt2(label string) {
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

	groupBlocks := func(g uint32) uint32 { return min(bpg, blocks-g*bpg) }
	usedIn := func(g uint32) uint32 {
		if g == 0 {
			return overhead + 2 // root and lost+found directories
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
}

func setBits(b []byte, from, to uint32) {
	for i := from; i < to && i/8 < uint32(len(b)); i++ {
		b[i/8] |= 1 << (i % 8)
	}
}
