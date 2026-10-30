// Package cpio writes "newc" (SVR4, no CRC) archives, the format the Linux
// kernel expects for an initramfs.
package cpio

import (
	"fmt"
	"io"
)

const (
	ModeDir     = 0o040000
	ModeFile    = 0o100000
	ModeSymlink = 0o120000
	ModeCharDev = 0o020000
)

type Writer struct {
	w     io.Writer
	n     int64
	inode uint32
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w, inode: 1} }

func (c *Writer) Dir(name string, perm uint32) error {
	return c.entry(name, ModeDir|perm, 0, 0, nil)
}

func (c *Writer) File(name string, perm uint32, data []byte) error {
	return c.entry(name, ModeFile|perm, 0, 0, data)
}

func (c *Writer) Symlink(name, target string) error {
	return c.entry(name, ModeSymlink|0o777, 0, 0, []byte(target))
}

func (c *Writer) CharDev(name string, perm uint32, major, minor uint32) error {
	return c.entry(name, ModeCharDev|perm, major, minor, nil)
}

// Close writes the trailer entry.
func (c *Writer) Close() error {
	return c.entry("TRAILER!!!", 0, 0, 0, nil)
}

func (c *Writer) entry(name string, mode, rmajor, rminor uint32, data []byte) error {
	nlink := uint32(1)
	if mode&0o170000 == ModeDir {
		nlink = 2
	}
	ino := c.inode
	if name == "TRAILER!!!" {
		ino = 0
	}
	c.inode++
	hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino, mode, 0, 0, nlink, 0, len(data), 0, 0, rmajor, rminor, len(name)+1, 0)
	if err := c.write([]byte(hdr)); err != nil {
		return err
	}
	if err := c.write(append([]byte(name), 0)); err != nil {
		return err
	}
	if err := c.pad(); err != nil {
		return err
	}
	if err := c.write(data); err != nil {
		return err
	}
	return c.pad()
}

func (c *Writer) write(b []byte) error {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return err
}

func (c *Writer) pad() error {
	if r := c.n % 4; r != 0 {
		return c.write(make([]byte, 4-r))
	}
	return nil
}
