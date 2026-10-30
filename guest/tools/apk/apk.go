// Package apk reads Alpine packages: a concatenation of gzip-compressed tar
// streams (signature, control, data).
package apk

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"strings"
)

// File is a regular file or symlink extracted from a package.
type File struct {
	Name     string // without leading "/" or "./"
	Mode     int64
	Data     []byte
	Linkname string
}

// Extract returns every regular file and symlink for which keep(name) is true.
func Extract(r io.Reader, keep func(name string) bool) (map[string]File, error) {
	br := bufio.NewReader(r)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return nil, err
	}
	out := map[string]File{}
	for {
		zr.Multistream(false)
		tr := tar.NewReader(zr)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			name := strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/")
			if !keep(name) {
				continue
			}
			switch h.Typeflag {
			case tar.TypeReg:
				data, err := io.ReadAll(tr)
				if err != nil {
					return nil, err
				}
				out[name] = File{Name: name, Mode: h.Mode, Data: data}
			case tar.TypeSymlink:
				out[name] = File{Name: name, Mode: h.Mode, Linkname: h.Linkname}
			}
		}
		// Drain the rest of this gzip member and move to the next one.
		if _, err := io.Copy(io.Discard, zr); err != nil {
			return nil, err
		}
		if err := zr.Reset(br); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
	}
}
