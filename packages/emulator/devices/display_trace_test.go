package devices

import "testing"

// drawStamp draws an fbtick stamp for seq across rows y0..y0+7, the way
// guest/rootfs/usr/bin/fbtick does.
func drawStamp(d *Display, y0, seq int) {
	v := fbtickSignature | seq<<8 | (seq^0xf)<<12
	for y := y0; y < y0+8; y++ {
		for i := 0; i < 16; i++ {
			c := byte(0)
			if v>>i&1 != 0 {
				c = 0xff
			}
			for x := i * fbtickCell; x < (i+1)*fbtickCell; x++ {
				o := y*d.Stride + x*4
				d.fb[o], d.fb[o+1], d.fb[o+2], d.fb[o+3] = c, c, c, c
			}
		}
	}
}

func fbtickFrame(d *Display, top, bottom, body int) {
	for y := 8; y < 392; y++ {
		for x := 0; x < d.Width*4; x++ {
			d.fb[y*d.Stride+x] = byte(body)
		}
	}
	drawStamp(d, 0, top)
	drawStamp(d, 392, bottom)
}

func TestDisplayTrace(t *testing.T) {
	d := NewDisplay(make([]byte, 640*480*4), 640, 480, 640*4)
	tr := NewDisplayTrace(100)
	d.Trace = tr
	d.Flush() // initial full flush: no stamps

	fbtickFrame(d, 3, 3, 0x10)
	d.Flush()
	fbtickFrame(d, 4, 3, 0x20) // caught half copied
	d.Flush()
	fbtickFrame(d, 4, 4, 0x20)
	d.Flush()
	d.Flush()                  // nothing changed
	fbtickFrame(d, 7, 7, 0x30) // 5 and 6 never seen
	d.Flush()
	fbtickFrame(d, 1, 1, 0x40) // wraps around: 8..15, 0 missed
	d.Flush()

	if tr.Calls != 7 || tr.Changed != 6 {
		t.Errorf("calls %d changed %d, want 7 and 6", tr.Calls, tr.Changed)
	}
	if tr.Complete != 4 || tr.Torn != 1 || tr.Skipped != 2+9 {
		t.Errorf("complete %d torn %d skipped %d, want 4, 1, 11", tr.Complete, tr.Torn, tr.Skipped)
	}
	recs, dropped := tr.TakeRecords()
	if len(recs) != 6 || dropped != 0 {
		t.Fatalf("%d records (%d dropped), want 6", len(recs), dropped)
	}
	if r := recs[0]; r.Top != -1 || r.MinY != 0 || r.MaxY != 479 {
		t.Errorf("first record %+v, want no stamp and the whole screen", r)
	}
	// The torn frame changed only the top stamp and the body.
	if r := recs[2]; r.Top != 4 || r.Bottom != 3 || r.MinY != 0 || r.MaxY != 399 {
		t.Errorf("torn record %+v", r)
	}
	// A stamp-free picture with bright pixels must not decode.
	for i := range d.fb {
		d.fb[i] = 0xff
	}
	if s := d.stamp(fbtickTopRow); s != -1 {
		t.Errorf("white screen decoded as stamp %d", s)
	}
}
