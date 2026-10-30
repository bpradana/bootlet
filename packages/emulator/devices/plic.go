package devices

// PLIC is a RISC-V platform-level interrupt controller with 31 level-triggered
// sources and two contexts: 0 = hart 0 M-mode, 1 = hart 0 S-mode.
type PLIC struct {
	priority  [32]uint32
	level     uint32 // current source input levels
	claimed   uint32 // sources claimed and not yet completed
	enable    [2]uint32
	threshold [2]uint32

	// Line is called with the context and its new external interrupt level.
	Line func(ctx int, level bool)
}

const NumSources = 32

func NewPLIC() *PLIC { return &PLIC{} }

// SetLevel sets the input level of an interrupt source.
func (p *PLIC) SetLevel(src int, high bool) {
	if high {
		p.level |= 1 << src
	} else {
		p.level &^= 1 << src
	}
	p.update()
}

func (p *PLIC) pending() uint32 { return p.level &^ p.claimed &^ 1 }

// best returns the highest-priority pending source enabled for ctx above
// its threshold, or 0.
func (p *PLIC) best(ctx int) uint32 {
	cand := p.pending() & p.enable[ctx]
	var best, bestPrio uint32
	for s := uint32(1); cand>>s != 0; s++ {
		if cand&(1<<s) != 0 && p.priority[s] > p.threshold[ctx] && p.priority[s] > bestPrio {
			best, bestPrio = s, p.priority[s]
		}
	}
	return best
}

func (p *PLIC) update() {
	if p.Line == nil {
		return
	}
	for ctx := 0; ctx < 2; ctx++ {
		p.Line(ctx, p.best(ctx) != 0)
	}
}

func (p *PLIC) Read(off uint64, _ int) (uint64, bool) {
	switch {
	case off < 0x1000:
		return uint64(p.priority[off/4%NumSources]), true
	case off == 0x1000:
		return uint64(p.pending()), true
	case off >= 0x2000 && off < 0x2000+0x80*2:
		ctx := (off - 0x2000) / 0x80
		if (off-0x2000)%0x80 == 0 {
			return uint64(p.enable[ctx]), true
		}
		return 0, true
	case off >= 0x200000 && off < 0x200000+0x1000*2:
		ctx := (off - 0x200000) / 0x1000
		switch off % 0x1000 {
		case 0:
			return uint64(p.threshold[ctx]), true
		case 4: // claim
			s := p.best(int(ctx))
			if s != 0 {
				p.claimed |= 1 << s
				p.update()
			}
			return uint64(s), true
		}
	}
	return 0, true
}

func (p *PLIC) Write(off uint64, _ int, v uint64) bool {
	switch {
	case off < 0x1000:
		p.priority[off/4%NumSources] = uint32(v) & 7
	case off >= 0x2000 && off < 0x2000+0x80*2:
		ctx := (off - 0x2000) / 0x80
		if (off-0x2000)%0x80 == 0 {
			p.enable[ctx] = uint32(v) &^ 1
		}
	case off >= 0x200000 && off < 0x200000+0x1000*2:
		ctx := (off - 0x200000) / 0x1000
		switch off % 0x1000 {
		case 0:
			p.threshold[ctx] = uint32(v) & 7
		case 4: // complete
			if v < NumSources {
				p.claimed &^= 1 << v
			}
		}
	default:
		return true
	}
	p.update()
	return true
}
