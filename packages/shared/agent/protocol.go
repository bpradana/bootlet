// Package agent is the protocol between the host and bootlet-agent, the
// guest program that serves the file explorer. They talk over a virtio
// console port, a plain byte stream.
//
// Each message is a frame: the magic "BLA1", the header length and payload
// length (u32, little endian), a JSON header (Request or Reply) and the
// payload bytes (file contents for read and write). The host sends requests
// one at a time or pipelined; the agent answers each in order with the same
// id. When the agent starts it sends an unsolicited Reply with id 0, so a
// host that was waiting knows it is up (data sent to the port while no agent
// had it open was dropped by the guest kernel).
package agent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
)

const (
	Magic   = "BLA1"
	Version = 1

	// MaxHeader and MaxPayload bound a frame; a larger length means the
	// stream is out of sync. ChunkSize is the read/write size clients use.
	MaxHeader  = 8 << 20
	MaxPayload = 1 << 20
	ChunkSize  = 256 << 10
)

// Request is a frame from the host. Ops and their fields:
//
//	hello                         → Version, Home
//	list   Path                   → Entries (directories first, then by name)
//	stat   Path                   → Entry
//	read   Path, Off, Len         → payload (up to Len bytes), EOF
//	write  Path, Off, Trunc + payload; creates the file
//	mkdir  Path                   (with parents)
//	rename Path, To, Overwrite    (moves across filesystems too)
//	copy   Path, To, Overwrite    (directories recursively)
//	remove Path                   (directories recursively)
//	stats                         → Stats (for the System tab)
//
// Paths are absolute. Without Overwrite, rename and copy fail with code
// "exist" when To exists.
type Request struct {
	ID        uint32 `json:"id"`
	Op        string `json:"op"`
	Path      string `json:"path,omitempty"`
	To        string `json:"to,omitempty"`
	Off       int64  `json:"off,omitempty"`
	Len       int    `json:"len,omitempty"`
	Trunc     bool   `json:"trunc,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// Reply answers the request with the same ID. On failure OK is false, Err
// is the error message and Code classifies it: "exist", "notexist", "perm"
// or "" for anything else.
type Reply struct {
	ID      uint32  `json:"id"`
	OK      bool    `json:"ok"`
	Err     string  `json:"err,omitempty"`
	Code    string  `json:"code,omitempty"`
	Version int     `json:"version,omitempty"`
	Home    string  `json:"home,omitempty"`
	Entries []Entry `json:"entries,omitempty"`
	Entry   *Entry  `json:"entry,omitempty"`
	EOF     bool    `json:"eof,omitempty"`
	Stats   *Stats  `json:"stats,omitempty"`
}

// Entry describes a file. Type is "file", "dir", "link" or "other"; for a
// link, Target is its target and TargetDir reports whether it resolves to a
// directory. Mode holds the permission bits, Mtime is in Unix milliseconds.
type Entry struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
	Mode      uint32 `json:"mode"`
	Mtime     int64  `json:"mtime"`
	Target    string `json:"target,omitempty"`
	TargetDir bool   `json:"targetDir,omitempty"`
}

// Encode returns the frame for a header (Request or Reply) and payload.
func Encode(hdr any, payload []byte) []byte {
	h, err := json.Marshal(hdr)
	if err != nil {
		panic(err) // headers are plain structs
	}
	f := make([]byte, 12, 12+len(h)+len(payload))
	copy(f, Magic)
	binary.LittleEndian.PutUint32(f[4:], uint32(len(h)))
	binary.LittleEndian.PutUint32(f[8:], uint32(len(payload)))
	f = append(f, h...)
	return append(f, payload...)
}

// Decoder splits a byte stream into frames. Bytes that do not start a valid
// frame (such as the tail of a frame from a previous agent) are skipped up
// to the next magic.
type Decoder struct{ buf []byte }

// Feed appends bytes received from the stream.
func (d *Decoder) Feed(b []byte) { d.buf = append(d.buf, b...) }

// Next returns the next complete frame, if there is one. The returned
// slices stay valid after later calls.
func (d *Decoder) Next() (hdr, payload []byte, ok bool) {
	for {
		i := bytes.Index(d.buf, []byte(Magic))
		if i < 0 {
			// Keep a possible partial magic at the end.
			d.buf = d.buf[max(0, len(d.buf)-len(Magic)+1):]
			return nil, nil, false
		}
		d.buf = d.buf[i:]
		if len(d.buf) < 12 {
			return nil, nil, false
		}
		hl := int(binary.LittleEndian.Uint32(d.buf[4:]))
		pl := int(binary.LittleEndian.Uint32(d.buf[8:]))
		if hl == 0 || hl > MaxHeader || pl > MaxPayload {
			d.buf = d.buf[1:] // not a real frame start
			continue
		}
		if len(d.buf) < 12+hl+pl {
			return nil, nil, false
		}
		hdr = bytes.Clone(d.buf[12 : 12+hl])
		payload = bytes.Clone(d.buf[12+hl : 12+hl+pl])
		d.buf = d.buf[12+hl+pl:]
		if len(d.buf) == 0 {
			d.buf = nil
		}
		return hdr, payload, true
	}
}
