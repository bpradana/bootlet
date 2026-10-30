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

// FormatExt2 writes an empty ext2 filesystem (4 KiB blocks, one block group,
// root directory and lost+found) to the disk. BusyBox has no mkfs.ext2, so a
// fresh disk is formatted here instead of in the guest.
func (d *Disk) FormatExt2(label string) {
	const (
		bs           = BlockSize
		inodeSize    = 128
		firstIno     = 11
		bitmapBlk    = 2
		inodeMapBlk  = 3
		inodeTabBlk  = 4
		blocksPerGrp = 8 * bs
	)
	blocks := uint32(min(d.size/bs, blocksPerGrp))
	inodes := uint32(min(uint64(blocks)/4, 8*bs)) &^ 31
	inodeTabBlocks := inodes * inodeSize / bs
	rootBlk := inodeTabBlk + inodeTabBlocks
	lfBlk := rootBlk + 1
	used := lfBlk + 1
	now := uint32(time.Now().Unix())
	le := binary.LittleEndian

	// Superblock (at byte 1024 of block 0).
	sb := make([]byte, bs)
	s := sb[1024:]
	le.PutUint32(s[0:], inodes)
	le.PutUint32(s[4:], blocks)
	le.PutUint32(s[12:], blocks-used)
	le.PutUint32(s[16:], inodes-firstIno)
	le.PutUint32(s[20:], 0) // first data block
	le.PutUint32(s[24:], 2) // log2(block size) - 10
	le.PutUint32(s[28:], 2) // fragment size
	le.PutUint32(s[32:], blocksPerGrp)
	le.PutUint32(s[36:], blocksPerGrp)
	le.PutUint32(s[40:], inodes)
	le.PutUint32(s[48:], now)
	le.PutUint16(s[54:], 0xffff) // max mount count: no forced fsck
	le.PutUint16(s[56:], 0xef53)
	le.PutUint16(s[58:], 1) // clean
	le.PutUint16(s[60:], 1) // errors: continue
	le.PutUint32(s[64:], now)
	le.PutUint32(s[76:], 1) // revision 1 (dynamic)
	le.PutUint32(s[84:], firstIno)
	le.PutUint16(s[88:], inodeSize)
	le.PutUint32(s[96:], 0x2) // incompat: filetype in directory entries
	rand.Read(s[104:120])     // UUID
	copy(s[120:136], label)
	d.WriteAt(sb, 0)

	// Group descriptor.
	gd := make([]byte, bs)
	le.PutUint32(gd[0:], bitmapBlk)
	le.PutUint32(gd[4:], inodeMapBlk)
	le.PutUint32(gd[8:], inodeTabBlk)
	le.PutUint16(gd[12:], uint16(blocks-used))
	le.PutUint16(gd[14:], uint16(inodes-firstIno))
	le.PutUint16(gd[16:], 2) // directories
	d.WriteAt(gd, bs)

	// Bitmaps: mark used entries and the padding past the end as in use.
	bm := make([]byte, bs)
	setBits(bm, 0, used)
	setBits(bm, blocks, blocksPerGrp)
	d.WriteAt(bm, bitmapBlk*bs)
	im := make([]byte, bs)
	setBits(im, 0, firstIno)
	setBits(im, inodes, 8*bs)
	d.WriteAt(im, inodeMapBlk*bs)

	// Inodes 2 (root) and 11 (lost+found).
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
		d.WriteAt(in, uint64(inodeTabBlk)*bs+uint64(ino-1)*inodeSize)
	}
	dirInode(2, 0o40755, 3, rootBlk)
	dirInode(firstIno, 0o40700, 2, lfBlk)

	// Directory contents.
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
