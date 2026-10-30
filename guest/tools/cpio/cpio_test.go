package cpio

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Dir("bin", 0o755)
	w.File("bin/hello", 0o755, []byte("abc"))
	w.Symlink("bin/sh", "hello")
	w.Close()

	b := buf.Bytes()
	if len(b)%4 != 0 {
		t.Fatalf("archive length %d not 4-byte aligned", len(b))
	}
	if n := strings.Count(string(b), "070701"); n != 4 {
		t.Fatalf("found %d headers, want 4", n)
	}
	if !bytes.Contains(b, []byte("bin/hello\x00")) || !bytes.Contains(b, []byte("abc")) {
		t.Fatal("file entry missing")
	}
	if !bytes.HasSuffix(bytes.TrimRight(b, "\x00"), []byte("TRAILER!!!")) {
		t.Fatal("missing trailer")
	}
}
