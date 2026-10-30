// Package boot loads guest images into RAM: raw RISC-V Linux Images and
// ELF executables.
package boot

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
)

// Image describes a RISC-V Linux kernel Image header.
type Image struct {
	TextOffset uint64 // load offset from the start of RAM
	Size       uint64 // effective size including bss
}

// ParseImage validates and parses a raw RISC-V kernel Image header.
func ParseImage(data []byte) (Image, error) {
	if len(data) < 64 || string(data[0x38:0x3c]) != "RSC\x05" {
		return Image{}, errors.New("not a RISC-V Linux Image (bad magic)")
	}
	img := Image{
		TextOffset: binary.LittleEndian.Uint64(data[8:]),
		Size:       binary.LittleEndian.Uint64(data[16:]),
	}
	if img.Size < uint64(len(data)) {
		img.Size = uint64(len(data))
	}
	return img, nil
}

// ELF is a loaded ELF executable.
type ELF struct {
	Entry   uint64
	Symbols map[string]uint64
}

// LoadELF copies the PT_LOAD segments of an ELF file into ram, which is
// mapped at physical address base.
func LoadELF(ram []byte, base uint64, data []byte) (ELF, error) {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return ELF{}, err
	}
	if f.Machine != elf.EM_RISCV || f.Class != elf.ELFCLASS64 {
		return ELF{}, errors.New("not a RV64 ELF")
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		off := p.Paddr - base
		if p.Paddr < base || off+p.Memsz > uint64(len(ram)) {
			return ELF{}, fmt.Errorf("segment at %#x does not fit in RAM", p.Paddr)
		}
		if p.Filesz > 0 {
			if _, err := p.ReadAt(ram[off:off+p.Filesz], 0); err != nil {
				return ELF{}, err
			}
		}
		clear(ram[off+p.Filesz : off+p.Memsz])
	}
	syms := map[string]uint64{}
	if ss, err := f.Symbols(); err == nil {
		for _, s := range ss {
			syms[s.Name] = s.Value
		}
	}
	return ELF{Entry: f.Entry, Symbols: syms}, nil
}
