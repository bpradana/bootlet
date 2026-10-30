// Package fdt builds flattened device tree blobs (DTB, version 17).
package fdt

import "encoding/binary"

// Node is a device tree node. Property values are raw big-endian bytes; use
// the helper constructors to build them.
type Node struct {
	Name     string
	Props    []Prop
	Children []*Node
}

type Prop struct {
	Name  string
	Value []byte
}

func (n *Node) Add(name string, v []byte) *Node {
	n.Props = append(n.Props, Prop{name, v})
	return n
}

func (n *Node) Child(name string) *Node {
	c := &Node{Name: name}
	n.Children = append(n.Children, c)
	return c
}

func U32(vs ...uint32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint32(b[4*i:], v)
	}
	return b
}

func U64(vs ...uint64) []byte {
	b := make([]byte, 8*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint64(b[8*i:], v)
	}
	return b
}

// Str encodes one or more NUL-terminated strings (a string list).
func Str(ss ...string) []byte {
	var b []byte
	for _, s := range ss {
		b = append(append(b, s...), 0)
	}
	return b
}

func Empty() []byte { return nil }

const (
	tokBeginNode = 1
	tokEndNode   = 2
	tokProp      = 3
	tokEnd       = 9
)

// Encode serializes the tree rooted at root (whose name must be "").
func Encode(root *Node, bootCPU uint32) []byte {
	var strs []byte
	offsets := map[string]uint32{}
	strOff := func(s string) uint32 {
		if o, ok := offsets[s]; ok {
			return o
		}
		o := uint32(len(strs))
		strs = append(append(strs, s...), 0)
		offsets[s] = o
		return o
	}

	var st []byte
	u32 := func(v uint32) { st = binary.BigEndian.AppendUint32(st, v) }
	pad := func() {
		for len(st)%4 != 0 {
			st = append(st, 0)
		}
	}
	var emit func(n *Node)
	emit = func(n *Node) {
		u32(tokBeginNode)
		st = append(append(st, n.Name...), 0)
		pad()
		for _, p := range n.Props {
			u32(tokProp)
			u32(uint32(len(p.Value)))
			u32(strOff(p.Name))
			st = append(st, p.Value...)
			pad()
		}
		for _, c := range n.Children {
			emit(c)
		}
		u32(tokEndNode)
	}
	emit(root)
	u32(tokEnd)

	const headerSize = 40
	const rsvSize = 16 // one terminating (0, 0) entry
	offRsv := uint32(headerSize)
	offStruct := offRsv + rsvSize
	offStrings := offStruct + uint32(len(st))
	total := offStrings + uint32(len(strs))

	out := make([]byte, 0, total)
	for _, v := range []uint32{0xd00dfeed, total, offStruct, offStrings, offRsv, 17, 16, bootCPU,
		uint32(len(strs)), uint32(len(st))} {
		out = binary.BigEndian.AppendUint32(out, v)
	}
	out = append(out, make([]byte, rsvSize)...)
	out = append(out, st...)
	out = append(out, strs...)
	return out
}
