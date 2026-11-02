package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Home is the directory the file explorer opens first.
const Home = "/root"

// Serve answers requests from rw until it returns an error. It first sends
// the unsolicited ready reply (id 0).
func Serve(rw io.ReadWriter) error {
	if _, err := rw.Write(Encode(Reply{OK: true, Version: Version, Home: Home}, nil)); err != nil {
		return err
	}
	var dec Decoder
	buf := make([]byte, 64<<10)
	for {
		n, err := rw.Read(buf)
		if n > 0 {
			dec.Feed(buf[:n])
			for {
				hdr, payload, ok := dec.Next()
				if !ok {
					break
				}
				var req Request
				if json.Unmarshal(hdr, &req) != nil || req.Op == "" {
					continue // not a request (e.g. a stray reply)
				}
				rep, data := Handle(req, payload)
				if _, err := rw.Write(Encode(rep, data)); err != nil {
					return err
				}
			}
		}
		if err != nil {
			return err
		}
	}
}

// Handle carries out one request.
func Handle(req Request, payload []byte) (rep Reply, data []byte) {
	defer func() {
		if r := recover(); r != nil {
			rep, data = fail(req, fmt.Errorf("agent: %v", r)), nil
		}
	}()
	if req.Op != "hello" {
		for _, p := range []*string{&req.Path, &req.To} {
			if *p != "" {
				if !filepath.IsAbs(*p) {
					return fail(req, fmt.Errorf("%s: path is not absolute", *p)), nil
				}
				*p = filepath.Clean(*p)
			}
		}
	}
	rep = Reply{ID: req.ID, OK: true}
	var err error
	switch req.Op {
	case "hello":
		rep.Version, rep.Home = Version, Home
	case "list":
		rep.Entries, err = list(req.Path)
	case "stat":
		var e Entry
		e, err = stat(req.Path)
		rep.Entry = &e
	case "read":
		data, rep.EOF, err = read(req.Path, req.Off, req.Len)
	case "write":
		err = write(req.Path, req.Off, req.Trunc, payload)
	case "mkdir":
		err = os.MkdirAll(req.Path, 0o755)
	case "rename":
		err = move(req.Path, req.To, req.Overwrite)
	case "copy":
		err = copyPath(req.Path, req.To, req.Overwrite)
	case "remove":
		if req.Path == "/" {
			err = errors.New("refusing to remove /")
		} else if _, err = os.Lstat(req.Path); err == nil {
			err = os.RemoveAll(req.Path)
		}
	default:
		err = fmt.Errorf("unknown op %q", req.Op)
	}
	if err != nil {
		return fail(req, err), nil
	}
	return rep, data
}

func fail(req Request, err error) Reply {
	rep := Reply{ID: req.ID, Err: err.Error()}
	switch {
	case errors.Is(err, fs.ErrExist):
		rep.Code = "exist"
	case errors.Is(err, fs.ErrNotExist):
		rep.Code = "notexist"
	case errors.Is(err, fs.ErrPermission):
		rep.Code = "perm"
	}
	return rep
}

func entry(dir, name string, info fs.FileInfo) Entry {
	e := Entry{
		Name:  name,
		Type:  "other",
		Size:  info.Size(),
		Mode:  uint32(info.Mode().Perm()),
		Mtime: info.ModTime().UnixMilli(),
	}
	switch m := info.Mode(); {
	case m.IsRegular():
		e.Type = "file"
	case m.IsDir():
		e.Type = "dir"
	case m&fs.ModeSymlink != 0:
		e.Type = "link"
		p := filepath.Join(dir, name)
		e.Target, _ = os.Readlink(p)
		if t, err := os.Stat(p); err == nil {
			e.TargetDir = t.IsDir()
		}
	}
	return e
}

func list(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue // removed while listing
		}
		entries = append(entries, entry(dir, de.Name(), info))
	}
	isDir := func(e Entry) bool { return e.Type == "dir" || e.TargetDir }
	sort.Slice(entries, func(i, j int) bool {
		if a, b := isDir(entries[i]), isDir(entries[j]); a != b {
			return a
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

func stat(p string) (Entry, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return Entry{}, err
	}
	return entry(filepath.Dir(p), filepath.Base(p), info), nil
}

func read(p string, off int64, n int) ([]byte, bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if n <= 0 || n > MaxPayload {
		n = MaxPayload
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err == io.EOF {
		return buf[:got], true, nil
	}
	if err != nil {
		return nil, false, err
	}
	// A full read that ends exactly at the end of the file also counts.
	info, err := f.Stat()
	return buf[:got], err == nil && off+int64(got) >= info.Size(), nil
}

func write(p string, off int64, trunc bool, data []byte) error {
	flags := os.O_WRONLY | os.O_CREATE
	if trunc {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(p, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, off); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// prepare checks the destination of a rename or copy: without overwrite it
// must not exist. With overwrite, an existing file is replaced in place, but
// a directory on either side means the destination is removed first. Moving
// or copying a directory into itself is refused.
func prepare(from, to string, overwrite bool) error {
	if from == to || strings.HasPrefix(to, from+"/") {
		return fmt.Errorf("cannot move or copy %s into itself", from)
	}
	src, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if dst, err := os.Lstat(to); err == nil {
		if !overwrite {
			return &fs.PathError{Op: "create", Path: to, Err: fs.ErrExist}
		}
		if src.IsDir() || dst.IsDir() {
			return os.RemoveAll(to)
		}
	}
	return nil
}

func move(from, to string, overwrite bool) error {
	if err := prepare(from, to, overwrite); err != nil {
		return err
	}
	err := os.Rename(from, to)
	// Directories that come from the read-only lower layer of the overlay
	// root (and anything across filesystems) cannot be renamed: copy them.
	if errors.Is(err, syscall.EXDEV) {
		if err = copyTree(from, to); err == nil {
			err = os.RemoveAll(from)
		}
	}
	return err
}

func copyPath(from, to string, overwrite bool) error {
	if err := prepare(from, to, overwrite); err != nil {
		return err
	}
	return copyTree(from, to)
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(to, strings.TrimPrefix(p, from))
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, dst)
		case info.Mode().IsRegular():
			return copyFile(p, dst, info.Mode().Perm())
		}
		return nil // devices, sockets and pipes are skipped
	})
}

func copyFile(from, to string, perm fs.FileMode) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
