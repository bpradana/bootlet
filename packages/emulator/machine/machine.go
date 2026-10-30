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
	RTCBase   = 0x0010_1000

	// TimebaseHz is the frequency of the time CSR.
	TimebaseHz = 10_000_000

	DefaultRAM     = 128 << 20
	DefaultCmdline = "console=ttyS0 quiet loglevel=3 no4lvl unaligned_scalar_speed=fast cma=0 initcall_blacklist=jent_mod_init"
)

type Config struct {
	RAMSize uint64 // bytes; DefaultRAM if zero
	Cmdline string // DefaultCmdline if empty
}

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
	cfg   Config
	CPU   *cpu.CPU
	RAM   []byte
	UART  *devices.UART
	PLIC  *devices.PLIC
	CLINT *devices.CLINT
	Disk  *devices.VirtioBlock // nil unless AttachDisk was called
	bus   devices.Bus

	kernel, initrd []byte

	start    time.Time
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
	m := &Machine{cfg: cfg, RAM: make([]byte, cfg.RAMSize)}
	m.UART = devices.NewUART()
	m.PLIC = devices.NewPLIC()
	m.CLINT = devices.NewCLINT(m.now)
	m.bus.Map(CLINTBase, 0x10000, m.CLINT)
	m.bus.Map(PLICBase, 0x400_0000, m.PLIC)
	m.bus.Map(UARTBase, 0x100, m.UART)
	m.bus.Map(RTCBase, 0x1000, &devices.RTC{})
	m.CPU = cpu.New(m.RAM, RAMBase, &m.bus)
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
	m.Disk = &devices.VirtioBlock{Disk: d, RAM: m.RAM, RAMBase: RAMBase}
	m.Disk.IRQ = func(level bool) { m.PLIC.SetLevel(DiskIRQ, level) }
	m.bus.Map(DiskBase, 0x1000, m.Disk)
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
	m.start = time.Now()
	m.sbiTimer = ^uint64(0)
	m.halted, m.reboot = false, false

	m.CPU.Reset(entry)
	m.CPU.X[10] = 0 // hart id
	m.CPU.X[11] = dtbAddr
	m.CPU.EnterSupervisor(entry)
	return nil
}

// now returns the current value of the time CSR.
func (m *Machine) now() uint64 {
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
		res.Instructions += uint64(m.CPU.Run(chunk))
		if m.reboot {
			if err := m.Reset(); err != nil {
				m.halted = true
			}
			continue
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
			if now := m.now(); next > now {
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
