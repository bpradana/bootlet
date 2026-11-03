// Package machine assembles the CPU, RAM and devices into a virtual machine
// that boots a RISC-V Linux kernel, and exposes the API used by both the
// native runner and the WebAssembly frontend.
package machine

import (
	"errors"
	"fmt"
	"time"

	"bootlet/emulator/boot"
	"bootlet/emulator/cpu"
	"bootlet/emulator/devices"
	"bootlet/emulator/netstack"
)

// Physical memory map (QEMU virt compatible).
const (
	RAMBase   = 0x8000_0000
	CLINTBase = 0x0200_0000
	PLICBase  = 0x0c00_0000
	UARTBase  = 0x1000_0000
	UARTIRQ   = 10
	DiskBase  = 0x1000_1000
	DiskIRQ   = 1
	NetBase   = 0x1000_2000
	NetIRQ    = 2
	AgentBase = 0x1000_3000
	AgentIRQ  = 3
	InputBase = 0x1000_4000
	InputIRQ  = 4
	RTCBase   = 0x0010_1000

	// TimebaseHz is the frequency of the time CSR.
	TimebaseHz = 10_000_000

	DefaultRAM = 128 << 20

	// The framebuffer: a simple-framebuffer right after RAM, in x8r8g8b8.
	FBWidth  = 640
	FBHeight = 480
	FBStride = FBWidth * 4
	FBSize   = (FBStride*FBHeight + 0xfff) &^ 0xfff
	// DefaultCmdline shows the full boot log from the first instruction
	// (earlycon); the other options skip boot work that is pointless under
	// emulation. Append QuietCmdline for a silent boot.
	DefaultCmdline = "console=ttyS0 earlycon=sbi no4lvl unaligned_scalar_speed=fast cma=0 initcall_blacklist=jent_mod_init"
	QuietCmdline   = "quiet loglevel=3"
	// DisplayCmdline is appended when the machine has a display.
	DisplayCmdline = "fbcon=font:VGA8x8"
)

type Config struct {
	RAMSize uint64 // bytes; DefaultRAM if zero
	Cmdline string // DefaultCmdline if empty
	// Display adds a framebuffer (FBWidth x FBHeight) after RAM, described
	// to the guest as a simple-framebuffer.
	Display bool
	// Splash draws bootlet's power-on screen on the display at reset, until
	// the guest draws its own output.
	Splash bool
	// Deterministic derives guest time from the instruction count instead
	// of the host clock (DeterministicIPS instructions per second, skipping
	// ahead while the guest waits), and fixes the random seed and the
	// real-time clock, so the same inputs give the same run: for benchmarks
	// and for comparing execution engines.
	Deterministic bool
	// Engine selects how guest instructions execute: EngineBlocks (the
	// default) decodes straight-line blocks once and caches them;
	// EngineInterp is the plain interpreter, kept as the reference.
	Engine string
}

const (
	EngineBlocks = "blocks"
	EngineInterp = "interp"
)

// DeterministicIPS is the guest's notion of speed in deterministic mode.
const DeterministicIPS = 100_000_000

// deterministicEpoch is the wall-clock time a deterministic guest boots at.
var deterministicEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Result describes why Run returned.
type Result struct {
	// Halted is set once the guest powered off.
	Halted bool
	// Idle is set when the hart is waiting for an interrupt. IdleFor is the
	// time until the next timer event, or a negative value if only input can
	// wake the guest.
	Idle    bool
	IdleFor time.Duration
	// Instructions executed during this call.
	Instructions uint64
}

type Machine struct {
	cfg      Config
	CPU      *cpu.CPU
	RAM      []byte
	UART     *devices.UART
	PLIC     *devices.PLIC
	CLINT    *devices.CLINT
	Disk     *devices.VirtioBlock   // nil unless AttachDisk was called
	Net      *devices.VirtioNet     // nil unless AttachNet was called
	Agent    *devices.VirtioConsole // nil unless AttachAgent was called
	Keyboard *devices.VirtioInput   // nil unless AttachKeyboard was called
	// Display is the framebuffer, nil unless Config.Display is set.
	Display *devices.Display
	stack   *netstack.Stack
	bus     devices.Bus

	kernel, initrd []byte

	splash     *devices.Splash
	splashNext uint64        // time CSR value of the next splash frame
	handover   time.Duration // when the guest took over the display from the splash

	start    time.Time
	skipped  uint64 // deterministic mode: timebase ticks skipped while idle
	sbiTimer uint64
	halted   bool
	reboot   bool
}

func New(cfg Config) *Machine {
	if cfg.RAMSize == 0 {
		cfg.RAMSize = DefaultRAM
	}
	if cfg.Cmdline == "" {
		cfg.Cmdline = DefaultCmdline
	}
	if cfg.Display {
		// The kernel's other built-in console font is Terminus 16x32, which
		// leaves 40x15 characters on 640x480; 8x8 gives 80x60.
		cfg.Cmdline += " " + DisplayCmdline
	}
	size := cfg.RAMSize
	if cfg.Display {
		// The framebuffer lives in the same host slice as RAM, so the guest's
		// pixel stores take the CPU's RAM fast path. The device tree's memory
		// node covers RAMSize only, so the kernel never allocates from it.
		size += FBSize
	}
	m := &Machine{cfg: cfg, RAM: make([]byte, size)}
	if cfg.Display {
		m.Display = devices.NewDisplay(m.RAM[cfg.RAMSize:cfg.RAMSize+FBStride*FBHeight], FBWidth, FBHeight, FBStride)
	}
	m.UART = devices.NewUART()
	m.PLIC = devices.NewPLIC()
	m.CLINT = devices.NewCLINT(m.now)
	m.bus.Map(CLINTBase, 0x10000, m.CLINT)
	m.bus.Map(PLICBase, 0x400_0000, m.PLIC)
	m.bus.Map(UARTBase, 0x100, m.UART)
	rtc := &devices.RTC{}
	if cfg.Deterministic {
		rtc.Now = func() time.Time { return deterministicEpoch.Add(time.Duration(m.now()) * (1e9 / TimebaseHz)) }
	}
	m.bus.Map(RTCBase, 0x1000, rtc)
	m.CPU = cpu.New(m.RAM, RAMBase, &m.bus)
	m.CPU.UseBlocks(cfg.Engine != EngineInterp)
	m.CPU.Ecall = m.sbi
	m.UART.IRQ = func(level bool) { m.PLIC.SetLevel(UARTIRQ, level) }
	m.PLIC.Line = func(ctx int, level bool) {
		if ctx == 0 {
			m.CPU.SetPending(cpu.IntMEI, level)
		} else {
			m.CPU.SetPending(cpu.IntSEI, level)
		}
	}
	m.CLINT.Update = m.tick
	return m
}

// LoadKernel sets the kernel Image booted by Reset.
func (m *Machine) LoadKernel(data []byte) error {
	img, err := boot.ParseImage(data)
	if err != nil {
		return err
	}
	if img.TextOffset+img.Size > m.cfg.RAMSize/2 {
		return fmt.Errorf("kernel needs %d MiB, too large for %d MiB of RAM", (img.TextOffset+img.Size)>>20, m.cfg.RAMSize>>20)
	}
	m.kernel = data
	return nil
}

// AttachDisk adds a virtio block device backed by d. Call it before Reset.
func (m *Machine) AttachDisk(d *devices.Disk) {
	m.Disk = devices.NewVirtioBlock(d, m.RAM, RAMBase)
	m.Disk.IRQ = func(level bool) { m.PLIC.SetLevel(DiskIRQ, level) }
	m.Disk.Touch = m.CPU.InvalidateRange
	m.bus.Map(DiskBase, 0x1000, m.Disk)
}

// ReplaceDisk swaps the disk behind the attached block device, e.g. to wipe
// it. Call Reset afterwards so the guest starts from the new contents.
func (m *Machine) ReplaceDisk(d *devices.Disk) {
	m.Disk.Disk = d
}

// AttachNet adds a virtio network device connected to the user-mode network
// s. Call it before Reset.
func (m *Machine) AttachNet(s *netstack.Stack) {
	m.stack = s
	m.Net = devices.NewVirtioNet(netstack.GuestMAC, m.RAM, RAMBase)
	m.Net.IRQ = func(level bool) { m.PLIC.SetLevel(NetIRQ, level) }
	m.Net.Touch = m.CPU.InvalidateRange
	m.Net.TX = func(frame []byte) {
		s.Receive(frame)
		m.Net.Deliver(s.Frames()...)
	}
	m.bus.Map(NetBase, 0x1000, m.Net)
}

// AgentPort is the name of the guest's file agent port.
const AgentPort = "bootlet.agent"

// AttachAgent adds the virtio console port that the guest's file agent
// (bootlet-agent) talks over. Call it before Reset.
func (m *Machine) AttachAgent() {
	m.Agent = devices.NewVirtioConsole(AgentPort, m.RAM, RAMBase)
	m.Agent.IRQ = func(level bool) { m.PLIC.SetLevel(AgentIRQ, level) }
	m.Agent.Touch = m.CPU.InvalidateRange
	m.bus.Map(AgentBase, 0x1000, m.Agent)
}

// AttachKeyboard adds the virtio keyboard that the display's key events
// reach the guest through. Call it before Reset.
func (m *Machine) AttachKeyboard() {
	m.Keyboard = devices.NewVirtioInput("bootlet keyboard", m.RAM, RAMBase)
	m.Keyboard.IRQ = func(level bool) { m.PLIC.SetLevel(InputIRQ, level) }
	m.Keyboard.Touch = m.CPU.InvalidateRange
	m.bus.Map(InputBase, 0x1000, m.Keyboard)
}

// KeyEvent presses (down) or releases a key on the virtio keyboard; code is a
// Linux KEY_* code.
func (m *Machine) KeyEvent(code uint16, down bool) {
	if m.Keyboard == nil {
		return
	}
	m.Keyboard.Key(code, down)
	m.CPU.WFI = false
}

// AgentInput sends bytes to the guest's file agent.
func (m *Machine) AgentInput(b []byte) {
	m.Agent.Deliver(b)
	m.CPU.WFI = false
}

// AgentOutput returns and clears the bytes the file agent sent.
func (m *Machine) AgentOutput() []byte { return m.Agent.Output() }

// pollNet moves upstream events (data, closes) into the guest; delivering
// frames raises the device interrupt, which wakes a hart waiting in wfi.
func (m *Machine) pollNet() {
	if m.stack == nil {
		return
	}
	m.stack.Poll()
	if f := m.stack.Frames(); len(f) > 0 {
		m.Net.Deliver(f...)
	}
}

// LoadInitrd sets the initramfs passed to the kernel.
func (m *Machine) LoadInitrd(data []byte) { m.initrd = data }

// Reset (re)boots the machine from the loaded kernel and initrd.
func (m *Machine) Reset() error {
	if m.kernel == nil {
		return errors.New("no kernel loaded")
	}
	img, _ := boot.ParseImage(m.kernel)
	clear(m.RAM)
	m.splash, m.handover = nil, 0
	if m.Display != nil {
		m.Display.Invalidate()
		if m.cfg.Splash {
			m.splash = devices.NewSplash(m.Display, fmt.Sprintf("RV64GC  %d MIB RAM  %dX%d DISPLAY",
				m.cfg.RAMSize>>20, FBWidth, FBHeight))
			m.splashNext = 0
		}
	}
	entry := RAMBase + img.TextOffset
	copy(m.RAM[img.TextOffset:], m.kernel)

	// The DTB goes in the last 64 KiB of RAM, the initrd right below it.
	ramEnd := RAMBase + m.cfg.RAMSize
	dtbAddr := ramEnd - 0x10000
	var initrdStart, initrdEnd uint64
	if len(m.initrd) > 0 {
		initrdStart = (dtbAddr - uint64(len(m.initrd))) &^ 0xfff
		initrdEnd = initrdStart + uint64(len(m.initrd))
		if initrdStart < entry+img.Size {
			return errors.New("initrd does not fit in RAM")
		}
		copy(m.RAM[initrdStart-RAMBase:], m.initrd)
	}
	dtb := m.deviceTree(initrdStart, initrdEnd)
	if len(dtb) > 0x10000 {
		return errors.New("device tree too large")
	}
	copy(m.RAM[dtbAddr-RAMBase:], dtb)

	*m.UART = devices.UART{IRQ: m.UART.IRQ}
	*m.PLIC = devices.PLIC{Line: m.PLIC.Line}
	m.CLINT.MSIP, m.CLINT.MTimecmp = false, ^uint64(0)
	if m.Disk != nil {
		m.Disk.Reset()
	}
	if m.Net != nil {
		m.Net.Reset()
		m.stack.Reset()
	}
	if m.Agent != nil {
		m.Agent.Reset()
	}
	if m.Keyboard != nil {
		m.Keyboard.Reset()
	}
	m.start = time.Now()
	m.skipped = 0
	m.sbiTimer = ^uint64(0)
	m.halted, m.reboot = false, false

	m.CPU.Reset(entry)
	m.CPU.X[10] = 0 // hart id
	m.CPU.X[11] = dtbAddr
	m.CPU.EnterSupervisor(entry)
	return nil
}

// FBBase is the guest physical address of the framebuffer.
func (m *Machine) FBBase() uint64 { return RAMBase + m.cfg.RAMSize }

// now returns the current value of the time CSR.
func (m *Machine) now() uint64 {
	if m.cfg.Deterministic {
		return m.CPU.Cycles/(DeterministicIPS/TimebaseHz) + m.skipped
	}
	return uint64(time.Since(m.start).Nanoseconds()) / (1e9 / TimebaseHz)
}

// tick refreshes the time CSR and timer/software interrupt lines.
// CPU.Time doubles as the machine's notion of "now" until the next tick.
func (m *Machine) tick() {
	t := m.now()
	c := m.CPU
	c.Time = t
	c.SetPending(cpu.IntSTI, t >= m.sbiTimer)
	c.SetPending(cpu.IntMTI, t >= m.CLINT.MTimecmp)
	c.SetPending(cpu.IntMSI, m.CLINT.MSIP)
}

// Input queues bytes typed on the serial console.
func (m *Machine) Input(b []byte) {
	m.UART.Input(b)
	m.CPU.WFI = false
}

// Output returns and clears console output produced since the last call.
func (m *Machine) Output() []byte { return m.UART.Output() }

// Halted reports whether the guest has powered off.
func (m *Machine) Halted() bool { return m.halted }

// Splash animation: frames per second, and how long it may run before it
// stops animating if the guest never draws (it then stays as a still image).
const (
	splashFrame = TimebaseHz / 15
	splashMax   = 60 * TimebaseHz
)

// animateSplash advances the splash; the first guest write to the
// framebuffer ends it.
func (m *Machine) animateSplash() {
	t := m.CPU.Time
	if t < m.splashNext {
		return
	}
	m.splashNext = t + splashFrame
	drew := m.splash.Tick(time.Duration(t) * (1e9 / TimebaseHz))
	m.CPU.InvalidateRange(m.FBBase(), FBSize) // the splash writes guest memory
	if !drew {
		m.handover = time.Duration(t) * (1e9 / TimebaseHz)
		m.splash = nil
	} else if t > splashMax {
		m.splash = nil
	}
}

// SplashActive reports whether the splash is still being animated.
func (m *Machine) SplashActive() bool { return m.splash != nil }

// SplashHandover returns the time after reset at which the guest took over
// the display from the splash (0 if it has not).
func (m *Machine) SplashHandover() time.Duration { return m.handover }

// chunk is the number of instructions between timer/clock checks.
const chunk = 20000

// Run executes the guest for up to budget of wall-clock time. It returns
// early when the guest halts or goes idle.
func (m *Machine) Run(budget time.Duration) Result {
	var res Result
	// Time is measured in timebase ticks from a single monotonic clock read
	// per chunk; reading the wall clock is costly under WebAssembly.
	deadline := m.now() + uint64(budget/(1e9/TimebaseHz))
	for !m.halted {
		m.tick()
		if m.splash != nil {
			m.animateSplash()
		}
		m.pollNet()
		res.Instructions += uint64(m.CPU.Run(chunk))
		if m.reboot {
			if err := m.Reset(); err != nil {
				m.halted = true
			}
			continue
		}
		if m.CPU.WFI && m.stack != nil && m.stack.Busy() {
			continue // network data arrived; deliver it on the next pass
		}
		if m.CPU.WFI {
			// Nothing to do until the next enabled timer deadline or input.
			next := ^uint64(0)
			if m.CPU.Enabled()&cpu.IntSTI != 0 {
				next = m.sbiTimer
			}
			if m.CPU.Enabled()&cpu.IntMTI != 0 {
				next = min(next, m.CLINT.MTimecmp)
			}
			if next == ^uint64(0) {
				res.Idle, res.IdleFor = true, -1
				return res
			}
			if now := m.now(); next > now && m.cfg.Deterministic {
				m.skipped += next - now // nothing happens until then
				continue
			} else if next > now {
				res.Idle = true
				res.IdleFor = time.Duration(next-now) * (1e9 / TimebaseHz)
				return res
			}
			continue
		}
		if m.CPU.Time >= deadline {
			break
		}
	}
	res.Halted = m.halted
	return res
}
