package fdt

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncode(t *testing.T) {
	root := &Node{}
	root.Add("#address-cells", U32(2))
	root.Child("chosen").Add("bootargs", Str("console=ttyS0")).Add("x", U64(0x80000000))

	b := Encode(root, 0)
	be := binary.BigEndian
	if be.Uint32(b) != 0xd00dfeed {
		t.Fatal("bad magic")
	}
	if int(be.Uint32(b[4:])) != len(b) {
		t.Fatalf("totalsize %d != %d", be.Uint32(b[4:]), len(b))
	}
	offStruct, offStrings := be.Uint32(b[8:]), be.Uint32(b[12:])
	if offStruct%4 != 0 || be.Uint32(b[offStruct:]) != tokBeginNode {
		t.Fatal("struct block does not start with FDT_BEGIN_NODE")
	}
	strs := b[offStrings:]
	for _, name := range []string{"#address-cells", "bootargs", "x"} {
		if !bytes.Contains(strs, append([]byte(name), 0)) {
			t.Errorf("strings block missing %q", name)
		}
	}
	if be.Uint32(b[offStrings-4:]) != tokEnd {
		t.Error("struct block does not end with FDT_END")
	}
}
