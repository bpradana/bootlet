//go:build js && wasm

// Command wasm exposes the machine to JavaScript as globalThis.bootlet. It is
// driven from a Web Worker: JavaScript calls run() in short slices and
// delivers input between them, so Go never blocks the worker's event loop.
package main

import (
	"runtime"
	"runtime/debug"
	"syscall/js"
	"time"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
	"bootlet/emulator/netstack"
)

var (
	m    *machine.Machine
	disk *devices.Disk
	wisp *netstack.WispUpstream
)

func main() {
	// Wasm memory never shrinks, so keep the heap close to its live size.
	debug.SetGCPercent(50)
	api := js.Global().Get("Object").New()
	api.Set("create", js.FuncOf(create))
	api.Set("loadKernel", js.FuncOf(loadKernel))
	api.Set("loadInitrd", js.FuncOf(loadInitrd))
	api.Set("reset", js.FuncOf(reset))
	api.Set("run", js.FuncOf(run))
	api.Set("input", js.FuncOf(input))
	api.Set("attachDisk", js.FuncOf(attachDisk))
	api.Set("takeDirty", js.FuncOf(takeDirty))
	api.Set("diskSaved", js.FuncOf(diskSaved))
	api.Set("memory", js.FuncOf(memory))
	api.Set("resetDisk", js.FuncOf(resetDisk))
	api.Set("attachNet", js.FuncOf(attachNet))
	api.Set("netInput", js.FuncOf(netInput))
	api.Set("netOutput", js.FuncOf(netOutput))
	api.Set("netConnecting", js.FuncOf(netConnecting))
	api.Set("netDisconnected", js.FuncOf(netDisconnected))
	api.Set("attachAgent", js.FuncOf(attachAgent))
	api.Set("agentInput", js.FuncOf(agentInput))
	api.Set("agentOutput", js.FuncOf(agentOutput))
	api.Set("displayFlush", js.FuncOf(displayFlush))
	api.Set("displayInvalidate", js.FuncOf(displayInvalidate))
	api.Set("keyEvent", js.FuncOf(keyEvent))
	js.Global().Set("bootlet", api)
	select {}
}

func errValue(err error) any {
	if err != nil {
		return err.Error()
	}
	return nil
}

func bytesFrom(v js.Value) []byte {
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
}

// create(ramMiB: number, cmdline: string, quiet: boolean, opts?: {display, splash})
// An empty cmdline uses the default; quiet hides the kernel's boot log;
// display adds the framebuffer; splash shows the power-on screen on it.
func create(_ js.Value, args []js.Value) any {
	cfg := machine.Config{Cmdline: machine.DefaultCmdline}
	if len(args) > 0 && args[0].Truthy() {
		cfg.RAMSize = uint64(args[0].Int()) << 20
	}
	if len(args) > 1 && args[1].Truthy() {
		cfg.Cmdline = args[1].String()
	}
	if len(args) > 2 && args[2].Truthy() {
		cfg.Cmdline += " " + machine.QuietCmdline
	}
	if len(args) > 3 && args[3].Truthy() {
		cfg.Display = args[3].Get("display").Truthy()
		cfg.Splash = args[3].Get("splash").Truthy()
		cfg.Deterministic = args[3].Get("deterministic").Truthy()
		if e := args[3].Get("engine"); e.Truthy() {
			cfg.Engine = e.String()
		}
	}
	m = machine.New(cfg)
	if cfg.Display {
		m.AttachKeyboard()
	}
	return nil
}

// loadKernel(image: Uint8Array): string | null
func loadKernel(_ js.Value, args []js.Value) any {
	return errValue(m.LoadKernel(bytesFrom(args[0])))
}

// loadInitrd(cpio: Uint8Array)
func loadInitrd(_ js.Value, args []js.Value) any {
	m.LoadInitrd(bytesFrom(args[0]))
	return nil
}

// reset(): string | null
func reset(js.Value, []js.Value) any {
	return errValue(m.Reset())
}

// run(budgetMs: number): {output, halted, idle, idleMs, instructions}
func run(_ js.Value, args []js.Value) any {
	res := m.Run(time.Duration(args[0].Float() * float64(time.Millisecond)))
	out := m.Output()
	arr := js.Global().Get("Uint8Array").New(len(out))
	js.CopyBytesToJS(arr, out)
	idleMs := -1.0
	if res.IdleFor >= 0 {
		idleMs = float64(res.IdleFor) / float64(time.Millisecond)
	}
	return map[string]any{
		"output":       arr,
		"halted":       res.Halted,
		"idle":         res.Idle,
		"idleMs":       idleMs,
		"instructions": float64(res.Instructions),
	}
}

// input(data: Uint8Array)
func input(_ js.Value, args []js.Value) any {
	m.Input(bytesFrom(args[0]))
	return nil
}

// jsBacking reads persisted disk blocks through a synchronous JavaScript
// function (an OPFS sync access handle in the worker).
type jsBacking struct{ read js.Value }

func (b jsBacking) ReadBlocks(idx []uint64, dst []byte) {
	ids := js.Global().Get("Uint32Array").New(len(idx))
	for i, n := range idx {
		ids.SetIndex(i, n)
	}
	out := js.Global().Get("Uint8Array").New(len(dst))
	b.read.Invoke(ids, out)
	js.CopyBytesToGo(dst, out)
}

// backing is set when the disk is read on demand, so resetDisk can reuse it.
var backing devices.Backing

// attachDisk(sizeMiB, source) adds the persistent disk. Call before reset().
// source is either {empty, readBlocks(idx: Uint32Array, out: Uint8Array)}:
// blocks are read from storage on demand and only unsaved ones stay in
// memory; or {idx: Uint32Array, data: Uint8Array}: every stored block is
// restored into memory (block idx[i] is data[i*4096:]). An empty store gets
// a freshly formatted filesystem.
func attachDisk(_ js.Value, args []js.Value) any {
	size := uint64(args[0].Int()) << 20
	src := args[1]
	if read := src.Get("readBlocks"); read.Type() == js.TypeFunction {
		backing = jsBacking{read}
		disk = devices.NewBackedDisk(size, backing)
		if src.Get("empty").Bool() {
			disk.Format("bootlet")
		}
	} else {
		disk = devices.NewDisk(size)
		n := src.Get("idx").Get("length").Int()
		if n == 0 {
			disk.Format("bootlet")
		} else {
			ids := src.Get("idx")
			data := bytesFrom(src.Get("data")) // one copy; blocks are sub-slices of it
			for i := 0; i < n; i++ {
				disk.Restore(uint64(ids.Index(i).Int()), data[i*devices.BlockSize:(i+1)*devices.BlockSize])
			}
		}
	}
	m.AttachDisk(disk)
	return nil
}

// diskSaved() confirms that the batch from the oldest unconfirmed
// takeDirty() was persisted, so it can be dropped from memory.
func diskSaved(js.Value, []js.Value) any {
	if disk != nil {
		disk.Saved()
	}
	return nil
}

// memory(): number returns the emulator's memory use in MiB, as WebAssembly
// memory obtained from the browser.
func memory(js.Value, []js.Value) any {
	var s runtime.MemStats
	runtime.ReadMemStats(&s)
	return float64(s.Sys >> 20)
}

// takeDirty(): {idx: Uint32Array, data: Uint8Array} | null returns the disk
// blocks changed since the last call.
func takeDirty(js.Value, []js.Value) any {
	if disk == nil {
		return nil
	}
	idx, data := disk.TakeDirty()
	if len(idx) == 0 {
		return nil
	}
	jsIdx := js.Global().Get("Uint32Array").New(len(idx))
	for i, n := range idx {
		jsIdx.SetIndex(i, n)
	}
	// Copy block by block: a flat Go copy of a large batch would be garbage
	// that wasm memory never gives back.
	jsData := js.Global().Get("Uint8Array").New(len(data) * devices.BlockSize)
	for i, b := range data {
		js.CopyBytesToJS(jsData.Call("subarray", i*devices.BlockSize, (i+1)*devices.BlockSize), b)
	}
	return map[string]any{"idx": jsIdx, "data": jsData}
}

// resetDisk(sizeMiB: number) replaces the disk with a freshly formatted one.
// Call reset() afterwards.
func resetDisk(_ js.Value, args []js.Value) any {
	size := uint64(args[0].Int()) << 20
	if backing != nil {
		disk = devices.NewBackedDisk(size, backing) // the caller cleared the store
	} else {
		disk = devices.NewDisk(size)
	}
	disk.Format("bootlet")
	m.ReplaceDisk(disk)
	return nil
}

// attachNet() adds a network device whose TCP streams are carried as Wisp
// packets; JavaScript moves them over the relay WebSocket. Call before reset().
func attachNet(js.Value, []js.Value) any {
	wisp = netstack.NewWispUpstream()
	m.AttachNet(netstack.New(wisp))
	return nil
}

// netInput(packet: Uint8Array) delivers one Wisp packet from the relay.
func netInput(_ js.Value, args []js.Value) any {
	wisp.Input(bytesFrom(args[0]))
	return nil
}

// netOutput(): Uint8Array[] returns the Wisp packets to send to the relay.
func netOutput(js.Value, []js.Value) any {
	pkts := wisp.Output()
	arr := js.Global().Get("Array").New(len(pkts))
	for i, p := range pkts {
		u := js.Global().Get("Uint8Array").New(len(p))
		js.CopyBytesToJS(u, p)
		arr.SetIndex(i, u)
	}
	return arr
}

// netConnecting() must be called when a WebSocket to the relay is opened:
// until then the guest's connections fail immediately.
func netConnecting(js.Value, []js.Value) any {
	wisp.Connecting()
	return nil
}

// netDisconnected() must be called when the relay WebSocket closes: open
// connections fail, and a new WebSocket starts a fresh Wisp session.
func netDisconnected(js.Value, []js.Value) any {
	wisp.Disconnected()
	return nil
}

// attachAgent() adds the console port of the guest's file agent, which
// serves the file explorer. Call before reset().
func attachAgent(js.Value, []js.Value) any {
	m.AttachAgent()
	return nil
}

// agentInput(data: Uint8Array) sends bytes to the file agent.
func agentInput(_ js.Value, args []js.Value) any {
	m.AgentInput(bytesFrom(args[0]))
	return nil
}

// agentOutput(): Uint8Array | null returns the bytes the file agent sent
// since the last call.
func agentOutput(js.Value, []js.Value) any {
	out := m.AgentOutput()
	if len(out) == 0 {
		return nil
	}
	arr := js.Global().Get("Uint8Array").New(len(out))
	js.CopyBytesToJS(arr, out)
	return arr
}

// displayFlush(out: Uint8Array): Int32Array | null copies the framebuffer
// regions changed since the last call into out as RGBA, and returns them as
// [x, y, w, h, offset] quintuples (offset into out, rows packed w*4 bytes).
// out must hold a whole screen.
func displayFlush(_ js.Value, args []js.Value) any {
	if m == nil || m.Display == nil {
		return nil
	}
	rects, data := m.Display.Flush()
	if len(rects) == 0 {
		return nil
	}
	js.CopyBytesToJS(args[0], data)
	r := js.Global().Get("Int32Array").New(5 * len(rects))
	for i, x := range rects {
		r.SetIndex(5*i, x.X)
		r.SetIndex(5*i+1, x.Y)
		r.SetIndex(5*i+2, x.W)
		r.SetIndex(5*i+3, x.H)
		r.SetIndex(5*i+4, x.Off)
	}
	return r
}

// displayInvalidate() makes the next displayFlush return the whole screen.
func displayInvalidate(js.Value, []js.Value) any {
	if m != nil && m.Display != nil {
		m.Display.Invalidate()
	}
	return nil
}

// keyEvent(code: number, down: boolean) presses or releases a key on the
// virtio keyboard (code is a Linux KEY_* code).
func keyEvent(_ js.Value, args []js.Value) any {
	if m != nil {
		m.KeyEvent(uint16(args[0].Int()), args[1].Bool())
	}
	return nil
}
