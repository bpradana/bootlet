package cpu

import (
	"time"
	"unsafe"
)

// WebAssembly JIT: the third execution tier, above the decoded-block cache.
//
// Blocks the block cache runs often are translated to WebAssembly functions,
// batched into modules that the host compiles (jit_js.go). A generated
// dispatcher loop then runs compiled blocks back to back without returning
// to Go: it checks for a pending interrupt and the instruction budget, looks
// up the current PC's translation in the instruction TLB, finds the compiled
// block in jitTab, and calls it through the shared function table.
//
// Generated code reads and writes the CPU state and guest RAM in place, at
// their addresses in the emulator's own WebAssembly memory (Go never moves
// heap objects). Everything it does not handle inline ends the run at that
// exact instruction with PC and instret up to date, and Go executes the
// instruction with the interpreter, so traps stay precise and interrupts are
// taken at the same instructions as with the interpreter and block cache:
//   - a TLB miss or a permission case beyond the common ones,
//   - an access outside RAM (MMIO) or across a page,
//   - a store to a page holding decoded code (Go invalidates it),
//   - anything but integer ALU, load/store and control-transfer instructions.
//
// Compiled blocks follow the block cache's invalidation: jitTab entries for a
// page are dropped with its blocks, and everything with a flush.

// jitBackend compiles and runs the generated modules (jit_js.go in the
// browser; there is none in native builds).
type jitBackend interface {
	// init instantiates the dispatcher module, with a function table of
	// tableN entries.
	init(dispatcher []byte, tableN uint32) error
	// load instantiates a module of compiled blocks, which installs them in
	// the function table.
	load(module []byte) error
	// run calls the dispatcher and returns its exit code.
	run() int32
}

const (
	jitTabSize = 1 << 16 // dispatcher hash table entries (16 B each)
	jitFuncs   = 1 << 14 // function table size
	jitBatch   = 128     // blocks per module
	jitMaxWait = 8       // Run calls before a partial batch is compiled
)

// Dispatcher and block exit codes.
const (
	jitContinue = 0 // (inside the dispatcher) run the next block
	jitStep     = 1 // execute the instruction at PC with the interpreter
	jitMiss     = 2 // no compiled block or translation for PC
	jitIRQ      = 3 // an interrupt is pending
	jitBudget   = 4 // the next block would exceed the instruction budget
	jitExits    = 5
)

// Block JIT states.
const (
	jitNone   = iota
	jitQueued // waiting in jitState.queue
	jitDone   // compiled; if Go runs it again, its jitTab entry was evicted
)

// jitHot is the number of block cache executions before a block is
// compiled.
var jitHot uint16 = 24

// jitEntry is read by the dispatcher: 16 bytes.
type jitEntry struct {
	key uint64 // RAM offset of the block + 1 (0: empty)
	fn  uint32 // function table index
	n   uint32 // most instructions the compiled block retires
}

type jitPending struct {
	key   uint64
	start uint32
	gen   uint32
	ops   []dop
}

type jitState struct {
	be      jitBackend
	tab     []jitEntry
	nextFn  uint32
	queue   []jitPending
	waiting int // Run calls since the queue became non-empty

	// Addresses of CPU state in WebAssembly memory.
	aX, aPC, aCycles, aMip, aMie, aLimit        uint32
	aMideleg, aMstatus                          uint32
	aFetchPaging, aPriv, aDataPaging, aDataPriv uint32
	aITLB, aDTLB, aRAM, aCodePage, aTab         uint32
	ramLen, ramBase                             uint64

	// Statistics.
	Compiled, Modules, Entries uint64
	Exits                      [jitExits]uint64
	CompileTime                time.Duration
	CodeBytes                  uint64
}

// JITStats reports the JIT counters (zero when it is off).
type JITStats struct {
	Compiled, Modules, Entries, CodeBytes uint64
	Exits                                 [jitExits]uint64
	CompileTime                           time.Duration
}

func (c *CPU) JITStats() JITStats {
	j := c.jit
	if j == nil {
		return JITStats{}
	}
	return JITStats{j.Compiled, j.Modules, j.Entries, j.CodeBytes, j.Exits, j.CompileTime}
}

// JITActive reports whether the JIT is enabled.
func (c *CPU) JITActive() bool { return c.jit != nil }

func addrOf(p unsafe.Pointer) uint32 { return uint32(uintptr(p)) }

// UseJIT enables the WebAssembly JIT on top of the block cache, if this build
// has a backend (the browser build). It reports whether the JIT is active.
func (c *CPU) UseJIT() bool {
	be := newJITBackend()
	if be == nil {
		return false
	}
	c.UseBlocks(true)
	j := &jitState{be: be, tab: make([]jitEntry, jitTabSize), nextFn: 1}
	j.aX = addrOf(unsafe.Pointer(&c.X[0]))
	j.aPC = addrOf(unsafe.Pointer(&c.PC))
	j.aCycles = addrOf(unsafe.Pointer(&c.Cycles))
	j.aMip = addrOf(unsafe.Pointer(&c.mip))
	j.aMie = addrOf(unsafe.Pointer(&c.mie))
	j.aLimit = addrOf(unsafe.Pointer(&c.jitLimit))
	j.aMideleg = addrOf(unsafe.Pointer(&c.mideleg))
	j.aMstatus = addrOf(unsafe.Pointer(&c.mstatus))
	j.aFetchPaging = addrOf(unsafe.Pointer(&c.fetchPaging))
	j.aPriv = addrOf(unsafe.Pointer(&c.Priv))
	j.aDataPaging = addrOf(unsafe.Pointer(&c.dataPaging))
	j.aDataPriv = addrOf(unsafe.Pointer(&c.dataPriv))
	j.aITLB = addrOf(unsafe.Pointer(&c.itlb[0]))
	j.aDTLB = addrOf(unsafe.Pointer(&c.dtlb[0]))
	j.aRAM = addrOf(unsafe.Pointer(&c.RAM[0]))
	j.aCodePage = addrOf(unsafe.Pointer(&c.blocks.codePage[0]))
	j.aTab = addrOf(unsafe.Pointer(&j.tab[0]))
	j.ramLen = uint64(len(c.RAM))
	j.ramBase = c.RAMBase
	if err := be.init(j.dispatcher(), jitFuncs); err != nil {
		return false
	}
	c.jit = j
	return true
}

// compiled reports whether the block at RAM offset off is compiled.
func (j *jitState) compiled(off uint64) bool {
	i := tableSet(off, jitTabSize)
	return j.tab[i].key == off+1 || j.tab[i+1].key == off+1
}

// slot picks where to put the compiled block at RAM offset off: its own
// slot, an empty one, or the second way (the most recent replacement goes
// there, keeping the first way for longer-lived code).
func (j *jitState) slot(off uint64) *jitEntry {
	i := tableSet(off, jitTabSize)
	for w := uint64(0); w < 2; w++ {
		if e := &j.tab[i+w]; e.key == off+1 || e.key == 0 {
			return e
		}
	}
	j.tab[i] = j.tab[i+1] // age the second way into the first
	return &j.tab[i+1]
}

// jitFlush forgets every compiled block (with a block cache flush).
func (j *jitState) flush() {
	clear(j.tab)
	j.queue = j.queue[:0]
	j.nextFn = 1
}

// jitInvalidatePage forgets the compiled blocks on RAM page p.
func (j *jitState) invalidatePage(p uint64) {
	for i := range j.tab {
		if e := &j.tab[i]; e.key != 0 && (e.key-1)>>12 == p {
			e.key = 0
		}
	}
	q := j.queue[:0]
	for _, b := range j.queue {
		if (b.key-1)>>12 != p {
			q = append(q, b)
		}
	}
	j.queue = q
}

// jitHit counts a block cache execution of e and queues hot blocks.
func (c *CPU) jitHit(e *blockEntry) {
	if e.hits < jitHot {
		e.hits++
		if e.hits < jitHot {
			return
		}
	}
	if e.jit == jitQueued {
		return
	}
	ops := c.blocks.ops[e.start : e.start+uint32(e.n)]
	n := compilable(ops)
	if !jitTranslates(ops[0].kind) {
		return // it would only exit again
	}
	e.jit = jitQueued
	j := c.jit
	j.queue = append(j.queue, jitPending{key: e.key, start: e.start, gen: e.gen, ops: append([]dop(nil), ops[:n]...)})
	if len(j.queue) >= jitBatch {
		c.jitCompile()
	}
}

// compilable returns how many leading ops of a block the JIT compiles: up
// to a control transfer, or up to and including the first op it does not
// translate, which becomes an exit to the interpreter at that instruction.
func compilable(ops []dop) int {
	for i, op := range ops {
		if !jitTranslates(op.kind) || op.kind >= kJAL {
			return i + 1
		}
	}
	return len(ops)
}

// jitTranslates reports whether generated code implements an op kind (the
// high multiplies need a 128-bit product, which WebAssembly lacks).
func jitTranslates(k uint8) bool {
	switch k {
	case kExec, kExecEnd, kMULH, kMULHSU, kMULHU:
		return false
	}
	return true
}

// jitCompile translates the queued blocks into one module.
func (c *CPU) jitCompile() {
	j := c.jit
	j.waiting = 0
	if len(j.queue) == 0 {
		return
	}
	start := time.Now()
	b := c.blocks
	if j.nextFn+uint32(len(j.queue)) > jitFuncs {
		j.flush() // out of table slots: start over
		return
	}
	m := wasmModule{
		types:    [][2][]wasmType{{{wI64}, {wI32}}},
		tableN:   jitFuncs,
		elemBase: j.nextFn,
	}
	var keep []jitPending
	for _, p := range j.queue {
		// Skip blocks invalidated or replaced since they were queued.
		e := b.find(p.key - 1)
		if e == nil || e.gen != p.gen || e.start != p.start {
			continue
		}
		e.jit = jitDone
		keep = append(keep, p)
		m.funcs = append(m.funcs, 0)
		m.elems = append(m.elems, uint32(len(m.elems)))
		m.bodies = append(m.bodies, j.translate(p.ops))
	}
	j.queue = j.queue[:0]
	if len(keep) == 0 {
		return
	}
	code := m.encode()
	if err := j.be.load(code); err != nil {
		return
	}
	for i, p := range keep {
		off := p.key - 1
		*j.slot(off) = jitEntry{key: p.key, fn: j.nextFn + uint32(i), n: uint32(len(p.ops))}
	}
	j.nextFn += uint32(len(keep))
	j.Compiled += uint64(len(keep))
	j.Modules++
	j.CodeBytes += uint64(len(code))
	j.CompileTime += time.Since(start)
}

// jitRunHook, if set (SetJITRun), calls the dispatcher through a
// WebAssembly import instead of syscall/js.
var jitRunHook func() int32

// SetJITRun installs a direct call to the dispatcher: a function declared
// with //go:wasmimport bootlet jit_run, whose host implementation calls
// globalThis.bootletJit.run (set by the JIT). It is much cheaper per call
// than syscall/js. The binary must then be run by a host that provides the
// import (the worker, scripts/go_js_wasm_exec_jit).
func SetJITRun(f func() int32) { jitRunHook = f }

// jitEnter runs compiled code from the current PC for at most budget
// instructions and returns how many it retired and the exit code.
func (c *CPU) jitEnter(budget int) (int, int32) {
	j := c.jit
	before := c.Cycles
	c.jitLimit = before + uint64(budget)
	j.Entries++
	var code int32
	if jitRunHook != nil {
		code = jitRunHook()
	} else {
		code = j.be.run()
	}
	if code >= 0 && code < jitExits {
		j.Exits[code]++
	}
	return int(c.Cycles - before), code
}

// ---- code generation ----

// Block function locals: the pc parameter, then scratch locals.
const (
	lPC = iota // i64 param: virtual address of the block
	lVA        // i64
	lPA        // i64
	lT         // i64
	lE         // i32
	lF         // i32
	lG         // i32
)

func (j *jitState) x(b *wbuf, r uint8) {
	if r == 0 {
		b.i64c(0)
		return
	}
	b.i32c(0)
	b.mem(opI64Load, 3, j.aX+8*uint32(r))
}

// setX stores the value value() leaves on the stack in x[rd] (nothing for x0).
func (j *jitState) setX(b *wbuf, rd uint8, value func()) {
	if rd == 0 {
		return
	}
	b.i32c(0)
	value()
	b.mem(opI64Store, 3, j.aX+8*uint32(rd))
}

func (j *jitState) store64(b *wbuf, addr uint32, value func()) {
	b.i32c(0)
	value()
	b.mem(opI64Store, 3, addr)
}

// exit sets PC to the instruction at byte offset delta, adds the k
// instructions retired so far to instret, and returns code.
func (j *jitState) exit(b *wbuf, code int32, k int, delta uint64) {
	j.store64(b, j.aPC, func() { b.get(lPC); b.i64c(delta); b.op(opI64Add) })
	j.retire(b, k)
	b.i32c(code)
	b.op(opReturn)
}

func (j *jitState) retire(b *wbuf, k int) {
	if k == 0 {
		return
	}
	j.store64(b, j.aCycles, func() {
		b.i32c(0)
		b.mem(opI64Load, 3, j.aCycles)
		b.i64c(uint64(k))
		b.op(opI64Add)
	})
}

func (j *jitState) translate(ops []dop) wasmFunc {
	f := wasmFunc{locals: []wasmType{wI64, wI64, wI64, wI32, wI32, wI32}}
	b := &f.code
	var delta uint64
	for k := range ops {
		op := &ops[k]
		next := delta + uint64(op.length)
		rd, rs1, rs2, imm := op.rd, op.rs1, op.rs2, op.imm
		bin := func(o byte) { j.setX(b, rd, func() { j.x(b, rs1); j.x(b, rs2); b.op(o) }) }
		bini := func(o byte) { j.setX(b, rd, func() { j.x(b, rs1); b.i64c(imm); b.op(o) }) }
		cmp := func(o byte, withImm bool) {
			j.setX(b, rd, func() {
				j.x(b, rs1)
				if withImm {
					b.i64c(imm)
				} else {
					j.x(b, rs2)
				}
				b.op(o, opI64ExtendU32)
			})
		}
		w32 := func(f func()) { // a 32-bit operation, sign-extended
			j.setX(b, rd, func() { f(); b.op(opI64ExtendS32) })
		}
		switch op.kind {
		case kLUI:
			j.setX(b, rd, func() { b.i64c(imm) })
		case kAUIPC:
			j.setX(b, rd, func() { b.get(lPC); b.i64c(delta + imm); b.op(opI64Add) })
		case kADDI:
			bini(opI64Add)
		case kSLTI:
			cmp(opI64LtS, true)
		case kSLTIU:
			cmp(opI64LtU, true)
		case kXORI:
			bini(opI64Xor)
		case kORI:
			bini(opI64Or)
		case kANDI:
			bini(opI64And)
		case kSLLI:
			bini(opI64Shl)
		case kSRLI:
			bini(opI64ShrU)
		case kSRAI:
			bini(opI64ShrS)
		case kADDIW:
			w32(func() { j.x(b, rs1); b.i64c(imm); b.op(opI64Add, opI32WrapI64) })
		case kSLLIW:
			w32(func() { j.x(b, rs1); b.op(opI32WrapI64); b.i32c(int32(rs2)); b.op(opI32Shl) })
		case kSRLIW:
			w32(func() { j.x(b, rs1); b.op(opI32WrapI64); b.i32c(int32(rs2)); b.op(opI32ShrU) })
		case kSRAIW:
			w32(func() { j.x(b, rs1); b.op(opI32WrapI64); b.i32c(int32(rs2)); b.op(opI32ShrS) })
		case kADD:
			bin(opI64Add)
		case kSUB:
			bin(opI64Sub)
		case kSLL:
			bin(opI64Shl)
		case kSLT:
			cmp(opI64LtS, false)
		case kSLTU:
			cmp(opI64LtU, false)
		case kXOR:
			bin(opI64Xor)
		case kSRL:
			bin(opI64ShrU)
		case kSRA:
			bin(opI64ShrS)
		case kOR:
			bin(opI64Or)
		case kAND:
			bin(opI64And)
		case kADDW:
			w32(func() { j.x(b, rs1); j.x(b, rs2); b.op(opI64Add, opI32WrapI64) })
		case kSUBW:
			w32(func() { j.x(b, rs1); j.x(b, rs2); b.op(opI64Sub, opI32WrapI64) })
		case kNOP:
		case kSLLW:
			w32(func() { j.x(b, rs1); b.op(opI32WrapI64); j.x(b, rs2); b.op(opI32WrapI64, opI32Shl) })
		case kSRLW:
			w32(func() { j.x(b, rs1); b.op(opI32WrapI64); j.x(b, rs2); b.op(opI32WrapI64, opI32ShrU) })
		case kSRAW:
			w32(func() { j.x(b, rs1); b.op(opI32WrapI64); j.x(b, rs2); b.op(opI32WrapI64, opI32ShrS) })
		case kMUL:
			bin(opI64Mul)
		case kMULW:
			w32(func() { j.x(b, rs1); j.x(b, rs2); b.op(opI64Mul, opI32WrapI64) })
		case kDIV, kDIVU, kREM, kREMU:
			j.setX(b, rd, func() { j.div64(b, op.kind, rs1, rs2) })
		case kDIVW, kDIVUW, kREMW, kREMUW:
			j.setX(b, rd, func() { j.div32(b, op.kind, rs1, rs2); b.op(opI64ExtendS32) })

		case kLB, kLH, kLW, kLD, kLBU, kLHU, kLWU:
			j.memAccess(b, op, k, delta, false)
		case kSB, kSH, kSW, kSD:
			j.memAccess(b, op, k, delta, true)

		case kJAL:
			j.setX(b, rd, func() { b.get(lPC); b.i64c(next); b.op(opI64Add) })
			j.store64(b, j.aPC, func() { b.get(lPC); b.i64c(delta + imm); b.op(opI64Add) })
			j.retire(b, k+1)
			b.i32c(jitContinue)
			b.op(opReturn)
			return f
		case kJALR:
			j.x(b, rs1)
			b.i64c(imm)
			b.op(opI64Add)
			b.i64c(^uint64(1))
			b.op(opI64And)
			b.set(lT)
			j.setX(b, rd, func() { b.get(lPC); b.i64c(next); b.op(opI64Add) })
			j.store64(b, j.aPC, func() { b.get(lT) })
			j.retire(b, k+1)
			b.i32c(jitContinue)
			b.op(opReturn)
			return f
		case kBEQ, kBNE, kBLT, kBGE, kBLTU, kBGEU:
			j.x(b, rs1)
			j.x(b, rs2)
			b.op(map[uint8]byte{kBEQ: opI64Eq, kBNE: opI64Ne, kBLT: opI64LtS, kBGE: opI64GeS, kBLTU: opI64LtU, kBGEU: opI64GeU}[op.kind])
			b.op(opIf, blockVoid)
			j.store64(b, j.aPC, func() { b.get(lPC); b.i64c(delta + imm); b.op(opI64Add) })
			b.op(opElse)
			j.store64(b, j.aPC, func() { b.get(lPC); b.i64c(next); b.op(opI64Add) })
			b.op(opEnd)
			j.retire(b, k+1)
			b.i32c(jitContinue)
			b.op(opReturn)
			return f
		default:
			// Not translated (compilable stops before these): exit to it.
			j.exit(b, jitStep, k, delta)
			return f
		}
		delta = next
	}
	// Ran off the end of the block (page end or block length).
	j.store64(b, j.aPC, func() { b.get(lPC); b.i64c(delta); b.op(opI64Add) })
	j.retire(b, len(ops))
	b.i32c(jitContinue)
	return f
}

// div64 leaves the RISC-V result of a 64-bit division on the stack: x/0 is
// all ones (div) or x (rem), and MinInt64/-1 is MinInt64 (div) or 0 (rem;
// WebAssembly's rem_s already gives 0 there, but div_s traps).
func (j *jitState) div64(b *wbuf, kind, rs1, rs2 uint8) {
	j.x(b, rs1)
	b.set(lVA)
	j.x(b, rs2)
	b.set(lPA)
	b.get(lPA)
	b.op(opI64Eqz, opIf, blockI64)
	if kind == kDIV || kind == kDIVU {
		b.i64c(^uint64(0))
	} else {
		b.get(lVA)
	}
	b.op(opElse)
	switch kind {
	case kDIV:
		b.get(lVA)
		b.i64c(1 << 63)
		b.op(opI64Eq)
		b.get(lPA)
		b.i64c(^uint64(0))
		b.op(opI64Eq, opI32And, opIf, blockI64)
		b.get(lVA)
		b.op(opElse)
		b.get(lVA)
		b.get(lPA)
		b.op(opI64DivS)
		b.op(opEnd)
	case kDIVU:
		b.get(lVA)
		b.get(lPA)
		b.op(opI64DivU)
	case kREM:
		b.get(lVA)
		b.get(lPA)
		b.op(opI64RemS)
	case kREMU:
		b.get(lVA)
		b.get(lPA)
		b.op(opI64RemU)
	}
	b.op(opEnd)
}

// div32 is div64 for the W forms, leaving an i32 to sign-extend.
func (j *jitState) div32(b *wbuf, kind, rs1, rs2 uint8) {
	j.x(b, rs1)
	b.op(opI32WrapI64)
	b.set(lE)
	j.x(b, rs2)
	b.op(opI32WrapI64)
	b.set(lF)
	b.get(lF)
	b.op(opI32Eqz, opIf, blockI32)
	if kind == kDIVW || kind == kDIVUW {
		b.i32c(-1)
	} else {
		b.get(lE)
	}
	b.op(opElse)
	switch kind {
	case kDIVW:
		b.get(lE)
		b.i32c(-1 << 31)
		b.op(opI32Eq)
		b.get(lF)
		b.i32c(-1)
		b.op(opI32Eq, opI32And, opIf, blockI32)
		b.get(lE)
		b.op(opElse)
		b.get(lE)
		b.get(lF)
		b.op(opI32DivS)
		b.op(opEnd)
	case kDIVUW:
		b.get(lE)
		b.get(lF)
		b.op(opI32DivU)
	case kREMW:
		b.get(lE)
		b.get(lF)
		b.op(opI32RemS)
	case kREMUW:
		b.get(lE)
		b.get(lF)
		b.op(opI32RemU)
	}
	b.op(opEnd)
}

var (
	accessSize = map[uint8]uint64{kLB: 1, kLH: 2, kLW: 4, kLD: 8, kLBU: 1, kLHU: 2, kLWU: 4, kSB: 1, kSH: 2, kSW: 4, kSD: 8}
	loadOp     = map[uint8]byte{kLB: opI64Load8S, kLH: opI64Load16S, kLW: opI64Load32S, kLD: opI64Load, kLBU: opI64Load8U, kLHU: opI64Load16U, kLWU: opI64Load32U}
	storeOp    = map[uint8]byte{kSB: opI64Store8, kSH: opI64Store16, kSW: opI64Store32, kSD: opI64Store}
	alignOf    = map[uint64]uint32{1: 0, 2: 1, 4: 2, 8: 3}
)

// memAccess emits a load or store with the same fast path as CPU.load and
// CPU.store: a data TLB hit with the common permissions, into RAM, not
// crossing a page. Anything else exits to the interpreter at this
// instruction. Stores to pages holding decoded code exit too, so Go
// invalidates them.
func (j *jitState) memAccess(b *wbuf, op *dop, k int, delta uint64, store bool) {
	size := accessSize[op.kind]
	// va = x[rs1] + imm
	j.x(b, op.rs1)
	b.i64c(op.imm)
	b.op(opI64Add)
	b.set(lVA)

	b.op(opBlock, blockVoid) // $done
	b.op(opBlock, blockVoid) // $slow

	// Translation: pa in lPA.
	b.i32c(0)
	b.mem(opI32Load8U, 0, j.aDataPaging)
	b.op(opIf, blockVoid)
	{
		// e = dtlb entry for va (inside if: $slow is depth 1)
		b.get(lVA)
		b.i64c(12)
		b.op(opI64ShrU)
		b.i64c(tlbSize - 1)
		b.op(opI64And, opI32WrapI64)
		b.i32c(int32(unsafe.Sizeof(tlbEntry{})))
		b.op(opI32Mul)
		b.tee(lE)
		b.mem(opI64Load, 3, j.aDTLB+uint32(unsafe.Offsetof(tlbEntry{}.tag)))
		b.get(lVA)
		b.i64c(12)
		b.op(opI64ShrU)
		b.op(opI64Ne, opBrIf, 1)

		b.get(lE)
		b.mem(opI32Load8U, 0, j.aDTLB+uint32(unsafe.Offsetof(tlbEntry{}.flags)))
		b.tee(lF)
		need := int32(pteR)
		if store {
			need = pteW | pteD
		}
		b.i32c(need)
		b.op(opI32And)
		b.i32c(need)
		b.op(opI32Ne, opBrIf, 1)

		// As CPU.permitted: U mode may only touch user pages; S mode may
		// touch them only with mstatus.SUM. (Loads from execute-only pages
		// with MXR take the slow path: they fail the R check above.)
		b.get(lF)
		b.i32c(pteU)
		b.op(opI32And, opI32Eqz, opI32Eqz)
		b.tee(lG) // 1 if a user page
		b.i32c(0)
		b.mem(opI32Load8U, 0, j.aDataPriv)
		b.op(opI32Eqz) // 1 if U mode
		b.op(opI32Xor) // a mismatch, ...
		b.get(lG)
		b.i32c(0)
		b.mem(opI64Load, 3, j.aMstatus)
		b.i64c(mstatusSUM)
		b.op(opI64And, opI64Eqz, opI32Eqz, opI32And) // ... unless S mode with SUM
		b.op(opI32Eqz, opI32And)
		b.op(opBrIf, 1)

		b.get(lE)
		b.mem(opI64Load, 3, j.aDTLB+uint32(unsafe.Offsetof(tlbEntry{}.page)))
		b.get(lVA)
		b.i64c(0xfff)
		b.op(opI64And, opI64Or)
		b.set(lPA)
	}
	b.op(opElse)
	b.get(lVA)
	b.set(lPA)
	b.op(opEnd)

	// off = pa - RAMBase, within RAM (as in CPU.load: off <= len-8) ...
	b.get(lPA)
	b.i64c(0 - j.ramBase)
	b.op(opI64Add)
	b.tee(lPA)
	b.i64c(j.ramLen - 8)
	b.op(opI64GtU, opBrIf, 0)
	// ... and not crossing the page.
	b.get(lVA)
	b.i64c(0xfff)
	b.op(opI64And)
	b.i64c(0x1000 - size)
	b.op(opI64GtU, opBrIf, 0)

	if store {
		// A page holding decoded code: let Go invalidate it.
		b.get(lPA)
		b.i64c(12)
		b.op(opI64ShrU, opI32WrapI64)
		b.mem(opI32Load8U, 0, j.aCodePage)
		b.op(opBrIf, 0)

		b.get(lPA)
		b.op(opI32WrapI64)
		j.x(b, op.rs2)
		b.mem(storeOp[op.kind], alignOf[size], j.aRAM)
	} else if op.rd != 0 {
		b.i32c(0)
		b.get(lPA)
		b.op(opI32WrapI64)
		b.mem(loadOp[op.kind], alignOf[size], j.aRAM)
		b.mem(opI64Store, 3, j.aX+8*uint32(op.rd))
	}
	b.op(opBr, 1) // $done
	b.op(opEnd)   // $slow:
	j.exit(b, jitStep, k, delta)
	b.op(opEnd) // $done
}

// dispatcher builds the module whose run() executes compiled blocks until
// one exits or nothing compiled is found.
func (j *jitState) dispatcher() []byte {
	f := wasmFunc{locals: []wasmType{wI64, wI64, wI32, wI32, wI64}}
	const (
		pc   = 0
		off  = 1
		e    = 2
		fl   = 3
		pend = 4
	)
	b := &f.code
	ret := func(code int32) { b.i32c(code); b.op(opReturn) }
	b.op(opLoop, blockVoid)
	{
		// An interrupt that CPU.interrupt would take now: let Go take it.
		// (A pending but disabled one must not end the run, or Go would
		// re-enter for every block.)
		b.i32c(0)
		b.mem(opI64Load, 3, j.aMip)
		b.i32c(0)
		b.mem(opI64Load, 3, j.aMie)
		b.op(opI64And)
		b.tee(pend)
		b.op(opI64Eqz, opI32Eqz, opIf, blockVoid)
		{
			// M-level: pending &^ mideleg, enabled below M or with MIE.
			b.get(pend)
			b.i32c(0)
			b.mem(opI64Load, 3, j.aMideleg)
			b.i64c(^uint64(0))
			b.op(opI64Xor, opI64And, opI64Eqz, opI32Eqz)
			b.i32c(0)
			b.mem(opI32Load8U, 0, j.aPriv)
			b.i32c(PrivM)
			b.op(opI32LtU)
			b.i32c(0)
			b.mem(opI64Load, 3, j.aMstatus)
			b.i64c(mstatusMIE)
			b.op(opI64And, opI64Eqz, opI32Eqz, opI32Or, opI32And)
			b.op(opIf, blockVoid)
			ret(jitIRQ)
			b.op(opEnd)
			// S-level: pending & mideleg, enabled in U, or in S with SIE.
			b.get(pend)
			b.i32c(0)
			b.mem(opI64Load, 3, j.aMideleg)
			b.op(opI64And, opI64Eqz, opI32Eqz)
			b.i32c(0)
			b.mem(opI32Load8U, 0, j.aPriv)
			b.i32c(PrivS)
			b.op(opI32LtU)
			b.i32c(0)
			b.mem(opI32Load8U, 0, j.aPriv)
			b.i32c(PrivS)
			b.op(opI32Eq)
			b.i32c(0)
			b.mem(opI64Load, 3, j.aMstatus)
			b.i64c(mstatusSIE)
			b.op(opI64And, opI64Eqz, opI32Eqz, opI32And, opI32Or, opI32And)
			b.op(opIf, blockVoid)
			ret(jitIRQ)
			b.op(opEnd)
		}
		b.op(opEnd)

		b.i32c(0)
		b.mem(opI64Load, 3, j.aPC)
		b.set(pc)

		// Instruction fetch translation through the instruction TLB.
		b.i32c(0)
		b.mem(opI32Load8U, 0, j.aFetchPaging)
		b.op(opIf, blockVoid)
		{
			b.get(pc)
			b.i64c(12)
			b.op(opI64ShrU)
			b.i64c(tlbSize - 1)
			b.op(opI64And, opI32WrapI64)
			b.i32c(int32(unsafe.Sizeof(tlbEntry{})))
			b.op(opI32Mul)
			b.tee(e)
			b.mem(opI64Load, 3, j.aITLB+uint32(unsafe.Offsetof(tlbEntry{}.tag)))
			b.get(pc)
			b.i64c(12)
			b.op(opI64ShrU)
			b.op(opI64Ne, opIf, blockVoid)
			ret(jitMiss)
			b.op(opEnd)

			b.get(e)
			b.mem(opI32Load8U, 0, j.aITLB+uint32(unsafe.Offsetof(tlbEntry{}.flags)))
			b.tee(fl)
			b.i32c(pteX)
			b.op(opI32And, opI32Eqz, opIf, blockVoid)
			ret(jitMiss)
			b.op(opEnd)
			b.get(fl)
			b.i32c(pteU)
			b.op(opI32And, opI32Eqz)
			b.i32c(0)
			b.mem(opI32Load8U, 0, j.aPriv)
			b.op(opI32Eqz)
			b.op(opI32Xor, opI32Eqz, opIf, blockVoid)
			ret(jitMiss)
			b.op(opEnd)

			b.get(e)
			b.mem(opI64Load, 3, j.aITLB+uint32(unsafe.Offsetof(tlbEntry{}.page)))
			b.get(pc)
			b.i64c(0xfff)
			b.op(opI64And, opI64Or)
			b.set(off)
		}
		b.op(opElse)
		b.get(pc)
		b.set(off)
		b.op(opEnd)

		b.get(off)
		b.i64c(0 - j.ramBase)
		b.op(opI64Add)
		b.tee(off)
		b.i64c(j.ramLen)
		b.op(opI64GeU, opIf, blockVoid)
		ret(jitMiss)
		b.op(opEnd)

		// The compiled block for this RAM offset: either way of its set
		// (see tableSet).
		b.get(off)
		b.i64c(1)
		b.op(opI64ShrU)
		b.get(off)
		b.i64c(17)
		b.op(opI64ShrU, opI64Xor)
		b.i64c(jitTabSize - 2)
		b.op(opI64And, opI32WrapI64)
		b.i32c(int32(unsafe.Sizeof(jitEntry{})))
		b.op(opI32Mul)
		b.tee(e)
		b.mem(opI64Load, 3, j.aTab+uint32(unsafe.Offsetof(jitEntry{}.key)))
		b.get(off)
		b.i64c(1)
		b.op(opI64Add)
		b.op(opI64Ne, opIf, blockVoid)
		{
			b.get(e)
			b.i32c(int32(unsafe.Sizeof(jitEntry{})))
			b.op(opI32Add)
			b.tee(e)
			b.mem(opI64Load, 3, j.aTab+uint32(unsafe.Offsetof(jitEntry{}.key)))
			b.get(off)
			b.i64c(1)
			b.op(opI64Add)
			b.op(opI64Ne, opIf, blockVoid)
			ret(jitMiss)
			b.op(opEnd)
		}
		b.op(opEnd)

		// It must fit the instruction budget.
		b.i32c(0)
		b.mem(opI64Load, 3, j.aCycles)
		b.get(e)
		b.mem(opI64Load32U, 2, j.aTab+uint32(unsafe.Offsetof(jitEntry{}.n)))
		b.op(opI64Add)
		b.i32c(0)
		b.mem(opI64Load, 3, j.aLimit)
		b.op(opI64GtU, opIf, blockVoid)
		ret(jitBudget)
		b.op(opEnd)

		b.get(pc)
		b.get(e)
		b.mem(opI32Load, 2, j.aTab+uint32(unsafe.Offsetof(jitEntry{}.fn)))
		b.op(opCallIndirect)
		b.u32(0) // type (i64) -> i32
		b.u32(0) // table
		b.tee(fl)
		b.op(opIf, blockVoid)
		b.get(fl)
		b.op(opReturn)
		b.op(opEnd)
		b.op(opBr, 0)
	}
	b.op(opEnd)
	b.i32c(jitMiss) // not reached
	m := wasmModule{
		types:   [][2][]wasmType{{{wI64}, {wI32}}, {nil, {wI32}}},
		funcs:   []uint32{1},
		bodies:  []wasmFunc{f},
		tableN:  jitFuncs,
		exports: map[string]uint32{"run": 0},
	}
	return m.encode()
}
