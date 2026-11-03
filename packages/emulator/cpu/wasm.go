package cpu

// A minimal WebAssembly binary encoder for the JIT (jit.go): just the
// sections, instructions and encodings the generated code uses.

type wasmType byte

const (
	wI32 wasmType = 0x7f
	wI64 wasmType = 0x7e
)

// Instruction opcodes.
const (
	opBlock        = 0x02
	opLoop         = 0x03
	opIf           = 0x04
	opElse         = 0x05
	opEnd          = 0x0b
	opBr           = 0x0c
	opBrIf         = 0x0d
	opReturn       = 0x0f
	opCallIndirect = 0x11
	opLocalGet     = 0x20
	opLocalSet     = 0x21
	opLocalTee     = 0x22
	opI32Load      = 0x28
	opI64Load      = 0x29
	opI32Load8U    = 0x2d
	opI64Load8S    = 0x30
	opI64Load8U    = 0x31
	opI64Load16S   = 0x32
	opI64Load16U   = 0x33
	opI64Load32S   = 0x34
	opI64Load32U   = 0x35
	opI64Store     = 0x37
	opI64Store8    = 0x3c
	opI64Store16   = 0x3d
	opI64Store32   = 0x3e
	opI32Const     = 0x41
	opI64Const     = 0x42
	opI32Eqz       = 0x45
	opI32Eq        = 0x46
	opI32Ne        = 0x47
	opI32LtU       = 0x49
	opI64Eqz       = 0x50
	opI64Eq        = 0x51
	opI64Ne        = 0x52
	opI64LtS       = 0x53
	opI64LtU       = 0x54
	opI64GtU       = 0x56
	opI64GeS       = 0x59
	opI64GeU       = 0x5a
	opSelect       = 0x1b
	opI32Add       = 0x6a
	opI32Mul       = 0x6c
	opI32DivS      = 0x6d
	opI32DivU      = 0x6e
	opI32RemS      = 0x6f
	opI32RemU      = 0x70
	opI32And       = 0x71
	opI32Or        = 0x72
	opI32Xor       = 0x73
	opI32Shl       = 0x74
	opI32ShrS      = 0x75
	opI32ShrU      = 0x76
	opI64Add       = 0x7c
	opI64Sub       = 0x7d
	opI64Mul       = 0x7e
	opI64DivS      = 0x7f
	opI64DivU      = 0x80
	opI64RemS      = 0x81
	opI64RemU      = 0x82
	opI64And       = 0x83
	opI64Or        = 0x84
	opI64Xor       = 0x85
	opI64Shl       = 0x86
	opI64ShrS      = 0x87
	opI64ShrU      = 0x88
	opI32WrapI64   = 0xa7
	opI64ExtendS32 = 0xac
	opI64ExtendU32 = 0xad

	blockVoid = 0x40
	blockI32  = 0x7f
	blockI64  = 0x7e
)

// wbuf accumulates encoded bytes.
type wbuf []byte

func (b *wbuf) op(ops ...byte) { *b = append(*b, ops...) }

func (b *wbuf) u32(v uint32) {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			*b = append(*b, c|0x80)
			continue
		}
		*b = append(*b, c)
		return
	}
}

func (b *wbuf) s64(v int64) {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0) {
			*b = append(*b, c)
			return
		}
		*b = append(*b, c|0x80)
	}
}

func (b *wbuf) i32c(v int32)  { b.op(opI32Const); b.s64(int64(v)) }
func (b *wbuf) i64c(v uint64) { b.op(opI64Const); b.s64(int64(v)) }
func (b *wbuf) get(l uint32)  { b.op(opLocalGet); b.u32(l) }
func (b *wbuf) set(l uint32)  { b.op(opLocalSet); b.u32(l) }
func (b *wbuf) tee(l uint32)  { b.op(opLocalTee); b.u32(l) }

// mem emits a load or store with the given alignment (log2) and offset.
func (b *wbuf) mem(op byte, align, offset uint32) { b.op(op); b.u32(align); b.u32(offset) }

func (b *wbuf) bytes(p []byte) { b.u32(uint32(len(p))); *b = append(*b, p...) }

func (b *wbuf) name(s string) { b.bytes([]byte(s)) }

func (b *wbuf) section(id byte, body wbuf) { b.op(id); b.bytes(body) }

// wasmFunc is a function body: its extra locals (after the parameters) and
// code, without the final end.
type wasmFunc struct {
	locals []wasmType
	code   wbuf
}

func (f *wasmFunc) encode() []byte {
	var b wbuf
	b.u32(uint32(len(f.locals)))
	for _, t := range f.locals {
		b.u32(1)
		b.op(byte(t))
	}
	b = append(b, f.code...)
	b.op(opEnd)
	var out wbuf
	out.bytes(b)
	return out
}

// wasmModule describes the modules the JIT emits: every one imports the
// emulator's memory as env.m and the shared function table as env.t.
type wasmModule struct {
	types   [][2][]wasmType // params, results
	funcs   []uint32        // type index per function
	bodies  []wasmFunc
	tableN  uint32 // minimum size of the imported table
	exports map[string]uint32
	// elemBase, if elems is set, places functions elems in the table from
	// index elemBase when the module is instantiated.
	elemBase uint32
	elems    []uint32
}

func (m *wasmModule) encode() []byte {
	b := wbuf{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	var types wbuf
	types.u32(uint32(len(m.types)))
	for _, t := range m.types {
		types.op(0x60)
		for _, list := range t {
			types.u32(uint32(len(list)))
			for _, v := range list {
				types.op(byte(v))
			}
		}
	}
	b.section(1, types)

	var imports wbuf
	imports.u32(2)
	imports.name("env")
	imports.name("m")
	imports.op(0x02, 0x00) // memory, no maximum
	imports.u32(0)
	imports.name("env")
	imports.name("t")
	imports.op(0x01, 0x70, 0x00) // table of funcref, no maximum
	imports.u32(m.tableN)
	b.section(2, imports)

	var funcs wbuf
	funcs.u32(uint32(len(m.funcs)))
	for _, t := range m.funcs {
		funcs.u32(t)
	}
	b.section(3, funcs)

	if len(m.exports) > 0 {
		var ex wbuf
		ex.u32(uint32(len(m.exports)))
		for name, idx := range m.exports {
			ex.name(name)
			ex.op(0x00)
			ex.u32(idx)
		}
		b.section(7, ex)
	}

	if len(m.elems) > 0 {
		var el wbuf
		el.u32(1)
		el.op(0x00) // active, table 0
		el.i32c(int32(m.elemBase))
		el.op(opEnd)
		el.u32(uint32(len(m.elems)))
		for _, f := range m.elems {
			el.u32(f)
		}
		b.section(9, el)
	}

	var code wbuf
	code.u32(uint32(len(m.bodies)))
	for i := range m.bodies {
		code = append(code, m.bodies[i].encode()...)
	}
	b.section(10, code)
	return b
}
