//go:build js && wasm

// Command wasm exposes the machine to JavaScript as globalThis.bootlet. It is
// driven from a Web Worker: JavaScript calls run() in short slices and
// delivers input between them, so Go never blocks the worker's event loop.
package main

import (
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
	api := js.Global().Get("Object").New()
	api.Set("create", js.FuncOf(create))
	api.Set("loadKernel", js.FuncOf(loadKernel))
	api.Set("loadInitrd", js.FuncOf(loadInitrd))
	api.Set("reset", js.FuncOf(reset))
	api.Set("run", js.FuncOf(run))
	api.Set("input", js.FuncOf(input))
	api.Set("attachDisk", js.FuncOf(attachDisk))
	api.Set("takeDirty", js.FuncOf(takeDirty))
	api.Set("resetDisk", js.FuncOf(resetDisk))
	api.Set("attachNet", js.FuncOf(attachNet))
	api.Set("netInput", js.FuncOf(netInput))
	api.Set("netOutput", js.FuncOf(netOutput))
	api.Set("netDisconnected", js.FuncOf(netDisconnected))
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

// create(ramMiB: number, cmdline: string, quiet: boolean). An empty
// cmdline uses the default; quiet hides the kernel's boot log.
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
	m = machine.New(cfg)
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

// attachDisk(sizeMiB: number, idx: Uint32Array, data: Uint8Array) restores a
// persisted disk (block idx[i] is data[i*4096:]) or formats a new one when
// idx is empty. Call before reset().
func attachDisk(_ js.Value, args []js.Value) any {
	disk = devices.NewDisk(uint64(args[0].Int()) << 20)
	n := args[1].Get("length").Int()
	if n == 0 {
		disk.FormatExt2("bootlet")
	} else {
		idx := bytesFrom(js.Global().Get("Uint8Array").New(args[1].Get("buffer"), args[1].Get("byteOffset"), n*4))
		data := bytesFrom(args[2])
		for i := 0; i < n; i++ {
			b := uint64(idx[4*i]) | uint64(idx[4*i+1])<<8 | uint64(idx[4*i+2])<<16 | uint64(idx[4*i+3])<<24
			disk.Restore(b, data[i*devices.BlockSize:(i+1)*devices.BlockSize])
		}
	}
	m.AttachDisk(disk)
	return nil
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
	flat := make([]byte, 0, len(data)*devices.BlockSize)
	for _, b := range data {
		flat = append(flat, b...)
	}
	jsData := js.Global().Get("Uint8Array").New(len(flat))
	js.CopyBytesToJS(jsData, flat)
	return map[string]any{"idx": jsIdx, "data": jsData}
}

// resetDisk(sizeMiB: number) replaces the disk with a freshly formatted one.
// Call reset() afterwards.
func resetDisk(_ js.Value, args []js.Value) any {
	disk = devices.NewDisk(uint64(args[0].Int()) << 20)
	disk.FormatExt2("bootlet")
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

// netDisconnected() must be called when the relay WebSocket closes: open
// connections fail, and a new WebSocket starts a fresh Wisp session.
func netDisconnected(js.Value, []js.Value) any {
	wisp.Disconnected()
	return nil
}
