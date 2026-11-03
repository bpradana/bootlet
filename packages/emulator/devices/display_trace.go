package devices

import "time"

// DisplayTrace records what each Display.Flush saw, to find where frames
// are lost between the guest and the screen. It is diagnostic only: a nil
// Display.Trace costs nothing.
//
// Besides dirty-tile statistics it decodes frame stamps drawn by the guest
// tool fbtick (guest/rootfs/usr/bin/fbtick): every fbtick frame carries the
// same 4-bit sequence number in a strip at its top and at its bottom. A
// flush that sees both stamps equal saw a complete frame; different stamps
// mean the frame was caught while it was still being copied in (torn).
type DisplayTrace struct {
	start time.Time

	Calls       uint64 // Flush calls
	Changed     uint64 // calls that found a change (what Display.Frames counts)
	FullChanges uint64 // changed calls with more than half the tiles dirty
	DirtyTiles  uint64 // sum over changed calls

	Complete uint64 // changed calls showing a new, untorn fbtick frame
	Torn     uint64 // changed calls with top stamp != bottom stamp
	Skipped  uint64 // fbtick frames missed between consecutive complete ones

	GPUFlushes    uint64 // virtio-gpu RESOURCE_FLUSH commands on the scanout
	GPUFlushRows  uint64 // rows they covered, summed
	GPUTransfers  uint64 // TRANSFER_TO_HOST_2D commands
	lastComplete  int
	maxRecords    int
	Records       []FlushRecord
	recordsHidden uint64 // records dropped once Records was full
}

// FlushRecord describes one Flush call that found a change.
type FlushRecord struct {
	At          time.Duration // since the trace started (host wall clock)
	Tiles       int           // dirty tiles
	MinY, MaxY  int           // first and last dirty pixel row (tile granularity)
	Top, Bottom int           // fbtick stamps, -1 if absent
	GPUFlushes  uint64        // DisplayTrace.GPUFlushes at this point
}

// NewDisplayTrace starts a trace that keeps up to maxRecords per-flush
// records (aggregates are always kept).
func NewDisplayTrace(maxRecords int) *DisplayTrace {
	return &DisplayTrace{start: time.Now(), maxRecords: maxRecords, lastComplete: -1}
}

// Since returns the time since the trace started.
func (t *DisplayTrace) Since() time.Duration { return time.Since(t.start) }

// TakeRecords returns the records collected since the last call and clears
// them, with the number dropped because the buffer was full.
func (t *DisplayTrace) TakeRecords() ([]FlushRecord, uint64) {
	r, n := t.Records, t.recordsHidden
	t.Records, t.recordsHidden = nil, 0
	return r, n
}

func (t *DisplayTrace) gpuFlush(rows uint32) {
	t.GPUFlushes++
	t.GPUFlushRows += uint64(rows)
}

// flush records a Flush call; dirty is the per-tile map, tx tiles per row.
func (t *DisplayTrace) flush(d *Display, any bool, dirty []bool, tx int) {
	t.Calls++
	if !any {
		return
	}
	t.Changed++
	n, minY, maxY := 0, -1, -1
	for i, ch := range dirty {
		if !ch {
			continue
		}
		n++
		y := i / tx
		if minY < 0 {
			minY = y
		}
		maxY = y
	}
	t.DirtyTiles += uint64(n)
	if 2*n > len(dirty) {
		t.FullChanges++
	}
	top, bot := d.stamp(fbtickTopRow), d.stamp(fbtickBottomRow)
	if top >= 0 && bot >= 0 {
		if top != bot {
			t.Torn++
		} else if top != t.lastComplete {
			t.Complete++
			if t.lastComplete >= 0 {
				t.Skipped += uint64((top - t.lastComplete - 1) & (fbtickPeriod - 1))
			}
			t.lastComplete = top
		}
	}
	r := FlushRecord{At: t.Since(), Tiles: n, MinY: minY * tileH, MaxY: min((maxY+1)*tileH, d.Height) - 1, Top: top, Bottom: bot, GPUFlushes: t.GPUFlushes}
	if minY < 0 {
		r.MinY, r.MaxY = -1, -1
	}
	if len(t.Records) < t.maxRecords {
		t.Records = append(t.Records, r)
	} else {
		t.recordsHidden++
	}
}

// fbtick's stamp layout; keep in sync with guest/rootfs/usr/bin/fbtick.
// A stamp is 16 cells of 40 pixels across one 8-row strip, each all white
// (bit 1) or all black (bit 0): bits 0-7 the signature 0xa5, bits 8-11 the
// sequence number and bits 12-15 its complement.
const (
	fbtickTopRow    = 4   // middle of rows 0-7
	fbtickBottomRow = 396 // middle of rows 392-399
	fbtickCell      = 40
	fbtickPeriod    = 16
	fbtickSignature = 0xa5
)

// stamp decodes the fbtick stamp crossing pixel row y, or returns -1.
func (d *Display) stamp(y int) int {
	if y >= d.Height || 16*fbtickCell > d.Width {
		return -1
	}
	v := 0
	for i := 0; i < 16; i++ {
		o := y*d.Stride + (i*fbtickCell+fbtickCell/2)*4
		switch g := d.fb[o+1]; { // green channel of x8r8g8b8
		case g >= 0xc0:
			v |= 1 << i
		case g > 0x40:
			return -1 // not a stamp cell
		}
	}
	seq := v >> 8 & 0xf
	if v&0xff != fbtickSignature || v>>12 != seq^0xf {
		return -1
	}
	return seq
}
