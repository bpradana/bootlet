package machine

import (
	"crypto/rand"

	"bootlet/emulator/fdt"
)

const (
	phandleCPUIntc = 1
	phandlePLIC    = 2
)

// deviceTree describes the machine to the kernel. The ISA string lists
// exactly the extensions the CPU implements so the kernel does not patch in
// instructions from extensions it was built for but that are missing here.
func (m *Machine) deviceTree(initrdStart, initrdEnd uint64) []byte {
	root := &fdt.Node{}
	root.Add("#address-cells", fdt.U32(2)).
		Add("#size-cells", fdt.U32(2)).
		Add("compatible", fdt.Str("bootlet,rv64", "riscv-virtio")).
		Add("model", fdt.Str("bootlet RV64GC"))

	chosen := root.Child("chosen")
	chosen.Add("bootargs", fdt.Str(m.cfg.Cmdline)).
		Add("stdout-path", fdt.Str("/soc/serial@10000000"))
	if initrdEnd > initrdStart {
		chosen.Add("linux,initrd-start", fdt.U64(initrdStart)).
			Add("linux,initrd-end", fdt.U64(initrdEnd))
	}
	seed := make([]byte, 64)
	rand.Read(seed)
	chosen.Add("rng-seed", seed)

	root.Child("memory@80000000").
		Add("device_type", fdt.Str("memory")).
		Add("reg", fdt.U64(RAMBase, m.cfg.RAMSize))

	cpus := root.Child("cpus")
	cpus.Add("#address-cells", fdt.U32(1)).
		Add("#size-cells", fdt.U32(0)).
		Add("timebase-frequency", fdt.U32(TimebaseHz))
	cpu0 := cpus.Child("cpu@0")
	cpu0.Add("device_type", fdt.Str("cpu")).
		Add("reg", fdt.U32(0)).
		Add("status", fdt.Str("okay")).
		Add("compatible", fdt.Str("riscv")).
		Add("riscv,isa", fdt.Str("rv64imafdc_zicntr_zicsr_zifencei")).
		Add("riscv,isa-base", fdt.Str("rv64i")).
		Add("riscv,isa-extensions", fdt.Str("i", "m", "a", "f", "d", "c", "zicntr", "zicsr", "zifencei")).
		Add("mmu-type", fdt.Str("riscv,sv39"))
	cpu0.Child("interrupt-controller").
		Add("#interrupt-cells", fdt.U32(1)).
		Add("interrupt-controller", fdt.Empty()).
		Add("compatible", fdt.Str("riscv,cpu-intc")).
		Add("phandle", fdt.U32(phandleCPUIntc))

	soc := root.Child("soc")
	soc.Add("#address-cells", fdt.U32(2)).
		Add("#size-cells", fdt.U32(2)).
		Add("compatible", fdt.Str("simple-bus")).
		Add("ranges", fdt.Empty())
	soc.Child("clint@2000000").
		Add("compatible", fdt.Str("sifive,clint0", "riscv,clint0")).
		Add("reg", fdt.U64(CLINTBase, 0x10000)).
		Add("interrupts-extended", fdt.U32(phandleCPUIntc, 3, phandleCPUIntc, 7))
	soc.Child("plic@c000000").
		Add("compatible", fdt.Str("sifive,plic-1.0.0", "riscv,plic0")).
		Add("reg", fdt.U64(PLICBase, 0x400_0000)).
		Add("#address-cells", fdt.U32(0)).
		Add("#interrupt-cells", fdt.U32(1)).
		Add("interrupt-controller", fdt.Empty()).
		Add("riscv,ndev", fdt.U32(31)).
		Add("interrupts-extended", fdt.U32(phandleCPUIntc, 11, phandleCPUIntc, 9)).
		Add("phandle", fdt.U32(phandlePLIC))
	soc.Child("serial@10000000").
		Add("compatible", fdt.Str("ns16550a")).
		Add("reg", fdt.U64(UARTBase, 0x100)).
		Add("clock-frequency", fdt.U32(3686400)).
		Add("interrupt-parent", fdt.U32(phandlePLIC)).
		Add("interrupts", fdt.U32(UARTIRQ))

	soc.Child("rtc@101000").
		Add("compatible", fdt.Str("google,goldfish-rtc")).
		Add("reg", fdt.U64(RTCBase, 0x1000)).
		Add("interrupt-parent", fdt.U32(phandlePLIC)).
		Add("interrupts", fdt.U32(11))

	if m.Disk != nil {
		soc.Child("virtio_mmio@10001000").
			Add("compatible", fdt.Str("virtio,mmio")).
			Add("reg", fdt.U64(DiskBase, 0x1000)).
			Add("interrupt-parent", fdt.U32(phandlePLIC)).
			Add("interrupts", fdt.U32(DiskIRQ))
	}

	if m.Net != nil {
		soc.Child("virtio_mmio@10002000").
			Add("compatible", fdt.Str("virtio,mmio")).
			Add("reg", fdt.U64(NetBase, 0x1000)).
			Add("interrupt-parent", fdt.U32(phandlePLIC)).
			Add("interrupts", fdt.U32(NetIRQ))
	}

	return fdt.Encode(root, 0)
}
