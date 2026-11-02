package agent

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Client makes requests one at a time over a byte stream to the agent.
type Client struct {
	// Send writes bytes to the agent.
	Send func([]byte)
	// Recv returns more bytes from the agent, blocking until there are some.
	Recv func() ([]byte, error)

	dec  Decoder
	next uint32
}

// Error is a failed request.
type Error struct{ Reply }

func (e *Error) Error() string { return e.Err }

// Call sends req (its ID is assigned) and waits for the reply. A failed
// request returns the reply and an *Error.
func (c *Client) Call(req Request, payload []byte) (Reply, []byte, error) {
	c.next++
	req.ID = c.next
	c.Send(Encode(req, payload))
	for {
		hdr, data, ok := c.dec.Next()
		if !ok {
			b, err := c.Recv()
			if err != nil {
				return Reply{}, nil, err
			}
			c.dec.Feed(b)
			continue
		}
		var rep Reply
		if err := json.Unmarshal(hdr, &rep); err != nil {
			return Reply{}, nil, fmt.Errorf("agent: bad reply: %w", err)
		}
		if rep.ID != req.ID {
			continue // the ready message, or a reply to an abandoned request
		}
		if !rep.OK {
			return rep, nil, &Error{rep}
		}
		return rep, data, nil
	}
}

// ReadFile reads a whole file in chunks.
func (c *Client) ReadFile(path string) ([]byte, error) {
	var out []byte
	for {
		rep, data, err := c.Call(Request{Op: "read", Path: path, Off: int64(len(out)), Len: ChunkSize}, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
		if rep.EOF {
			return out, nil
		}
		if len(data) == 0 {
			return nil, errors.New("agent: read made no progress")
		}
	}
}

// WriteFile writes a whole file in chunks.
func (c *Client) WriteFile(path string, data []byte) error {
	off := 0
	for first := true; first || off < len(data); first = false {
		n := min(ChunkSize, len(data)-off)
		if _, _, err := c.Call(Request{Op: "write", Path: path, Off: int64(off), Trunc: first}, data[off:off+n]); err != nil {
			return err
		}
		off += n
	}
	return nil
}
