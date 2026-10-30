// Package apk reads Alpine packages: a concatenation of gzip-compressed tar
// streams (signature, control, data).
package apk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
)

// File is an entry from a package's data section.
type File struct {
	Name     string // without leading "/" or "./"
	Mode     int64
	Dir      bool
	Data     []byte
	Linkname string // for symlinks
	SHA1     []byte // content checksum recorded by abuild, if any
}

// Package is a parsed .apk file.
type Package struct {
	Files []File              // data entries in archive order
	Info  map[string][]string // .PKGINFO fields (a key may repeat)
	// Checksum identifies the package in apk's installed database: "Q1" +
	// base64(SHA-1 of the compressed control section).
	Checksum string
	Size     int // size of the .apk file
}

// Read parses an apk. keep selects which data entries to load (nil keeps
// all); metadata is always read.
func Read(data []byte, keep func(name string) bool) (*Package, error) {
	pkg := &Package{Info: map[string][]string{}, Size: len(data)}
	r := bytes.NewReader(data) // a ByteReader, so gzip reads members exactly
	var sections [][]byte
	for r.Len() > 0 {
		start := len(data) - r.Len()
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		zr.Multistream(false)
		body, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		end := len(data) - r.Len()
		sections = append(sections, body)
		if len(sections) == 2 {
			sum := sha1.Sum(data[start:end])
			pkg.Checksum = "Q1" + base64.StdEncoding.EncodeToString(sum[:])
		}
	}
	for _, sec := range sections {
		tr := tar.NewReader(bytes.NewReader(sec))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/"), "/")
			if name == ".PKGINFO" {
				b, _ := io.ReadAll(tr)
				for _, line := range strings.Split(string(b), "\n") {
					if k, v, ok := strings.Cut(line, " = "); ok && !strings.HasPrefix(k, "#") {
						pkg.Info[k] = append(pkg.Info[k], v)
					}
				}
				continue
			}
			if strings.HasPrefix(name, ".") || name == "" || (keep != nil && !keep(name)) {
				continue // control files (.SIGN.*, scripts)
			}
			f := File{Name: name, Mode: h.Mode & 0o7777}
			if sum, err := hex.DecodeString(h.PAXRecords["APK-TOOLS.checksum.SHA1"]); err == nil && len(sum) > 0 {
				f.SHA1 = sum
			}
			switch h.Typeflag {
			case tar.TypeDir:
				f.Dir = true
			case tar.TypeSymlink:
				f.Linkname = h.Linkname
			case tar.TypeReg:
				if f.Data, err = io.ReadAll(tr); err != nil {
					return nil, err
				}
			default:
				continue
			}
			pkg.Files = append(pkg.Files, f)
		}
	}
	return pkg, nil
}

// Extract returns the regular files and symlinks for which keep(name) is
// true, by name.
func Extract(r io.Reader, keep func(name string) bool) (map[string]File, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	pkg, err := Read(data, keep)
	if err != nil {
		return nil, err
	}
	out := map[string]File{}
	for _, f := range pkg.Files {
		if !f.Dir {
			out[f.Name] = f
		}
	}
	return out, nil
}

// InstalledRecord renders the package's entry for /lib/apk/db/installed so
// apk treats it as installed.
func (p *Package) InstalledRecord() string {
	var b strings.Builder
	line := func(k, v string) {
		if v != "" {
			b.WriteString(k + ":" + v + "\n")
		}
	}
	first := func(k string) string {
		if v := p.Info[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	line("C", p.Checksum)
	line("P", first("pkgname"))
	line("V", first("pkgver"))
	line("A", first("arch"))
	line("S", strconv.Itoa(p.Size))
	line("I", first("size"))
	line("T", first("pkgdesc"))
	line("U", first("url"))
	line("L", first("license"))
	line("o", first("origin"))
	line("m", first("maintainer"))
	line("t", first("builddate"))
	line("c", first("commit"))
	line("D", strings.Join(p.Info["depend"], " "))
	line("p", strings.Join(p.Info["provides"], " "))

	// Files are listed under their directory: F:<dir> then R:<name> lines.
	dir := "\x00"
	for _, f := range p.Files {
		if f.Dir {
			dir = f.Name
			line("F", f.Name)
			if f.Mode != 0o755 {
				line("M", "0:0:"+strconv.FormatInt(f.Mode, 8))
			}
			continue
		}
		parent, base := "", f.Name
		if i := strings.LastIndex(f.Name, "/"); i >= 0 {
			parent, base = f.Name[:i], f.Name[i+1:]
		}
		if parent != dir {
			dir = parent
			line("F", parent)
		}
		line("R", base)
		if f.Linkname == "" && f.Mode != 0o644 {
			line("a", "0:0:"+strconv.FormatInt(f.Mode, 8))
		}
		if len(f.SHA1) > 0 {
			line("Z", "Q1"+base64.StdEncoding.EncodeToString(f.SHA1))
		}
	}
	b.WriteString("\n")
	return b.String()
}
