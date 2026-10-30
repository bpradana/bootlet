package cpu

import (
	"math"
	"math/big"
)

// Floating point (F and D extensions).
//
// Arithmetic is done with Go's native round-to-nearest-even operations
// together with an exact error sign obtained from error-free transformations
// (TwoSum, FMA residuals). The error sign gives the inexact flag and lets the
// directed rounding modes nudge the result by one ulp. Single precision
// operations are computed in double and rounded once more, which is exact for
// +, -, *, / and sqrt because 53 >= 2*24+2.

const (
	flagNX = 1 << 0
	flagUF = 1 << 1
	flagOF = 1 << 2
	flagDZ = 1 << 3
	flagNV = 1 << 4

	rmRNE = 0
	rmRTZ = 1
	rmRDN = 2
	rmRUP = 3
	rmRMM = 4

	nanBoxS = 0xffffffff00000000
	qnanS   = 0x7fc00000
	qnanD   = 0x7ff8000000000000
)

func (c *CPU) getS(r uint32) uint32 {
	v := c.F[r]
	if v>>32 != 0xffffffff {
		return qnanS
	}
	return uint32(v)
}

func (c *CPU) setS(r uint32, v uint32) {
	c.F[r] = nanBoxS | uint64(v)
	c.mstatus |= mstatusFS
}

func (c *CPU) setD(r uint32, v uint64) {
	c.F[r] = v
	c.mstatus |= mstatusFS
}

func (c *CPU) raise(flags uint64) {
	if flags != 0 {
		c.fflags |= flags
		c.mstatus |= mstatusFS
	}
}

func isNaNS(b uint32) bool  { return b&0x7f800000 == 0x7f800000 && b&0x7fffff != 0 }
func isSNaNS(b uint32) bool { return isNaNS(b) && b&0x400000 == 0 }
func isNaND(b uint64) bool {
	return b&0x7ff0000000000000 == 0x7ff0000000000000 && b&0xfffffffffffff != 0
}
func isSNaND(b uint64) bool { return isNaND(b) && b&0x8000000000000 == 0 }

func sign(x float64) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

// fp executes a floating point instruction. ok=false means illegal.
func (c *CPU) fp(insn, rd, rs1, rs2, f3 uint32) (ok, trapped bool) {
	if !c.fpEnabled() {
		return false, false
	}
	op := insn & 0x7f
	switch op {
	case 0x07: // flw, fld
		addr := c.X[rs1] + immI(insn)
		switch f3 {
		case 2:
			v, ok := c.load(addr, 4)
			if !ok {
				return true, true
			}
			c.setS(rd, uint32(v))
		case 3:
			v, ok := c.load(addr, 8)
			if !ok {
				return true, true
			}
			c.setD(rd, v)
		default:
			return false, false
		}
		return true, false
	case 0x27: // fsw, fsd
		addr := c.X[rs1] + immS(insn)
		switch f3 {
		case 2:
			if !c.store(addr, 4, c.F[rs2]) {
				return true, true
			}
		case 3:
			if !c.store(addr, 8, c.F[rs2]) {
				return true, true
			}
		default:
			return false, false
		}
		return true, false
	}

	fmt := insn >> 25 & 3
	if fmt > 1 {
		return false, false
	}
	rm := uint64(f3)
	if rm == 7 {
		rm = c.frm
	}

	if op != 0x53 { // fused multiply-add family
		if rm > rmRMM {
			return false, false
		}
		rs3 := insn >> 27
		negProd := op == 0x4b || op == 0x4f
		negAdd := op == 0x47 || op == 0x4f
		if fmt == 0 {
			a, b, d := c.getS(rs1), c.getS(rs2), c.getS(rs3)
			r := c.fmaS(a, b, d, negProd, negAdd, rm)
			c.setS(rd, r)
		} else {
			r := c.fmaD(c.F[rs1], c.F[rs2], c.F[rs3], negProd, negAdd, rm)
			c.setD(rd, r)
		}
		return true, false
	}

	funct5 := insn >> 27
	usesRM := funct5 <= 0x03 || funct5 == 0x0b || funct5 == 0x08 || funct5 == 0x18 || funct5 == 0x1a
	if usesRM && rm > rmRMM {
		return false, false
	}
	if fmt == 0 {
		return c.fpS(funct5, rd, rs1, rs2, f3, rm), false
	}
	return c.fpD(funct5, rd, rs1, rs2, f3, rm), false
}

// --- double precision ---

func (c *CPU) fpD(funct5, rd, rs1, rs2, f3 uint32, rm uint64) bool {
	ab, bb := c.F[rs1], c.F[rs2]
	a, b := math.Float64frombits(ab), math.Float64frombits(bb)
	switch funct5 {
	case 0x00, 0x01, 0x02, 0x03: // fadd, fsub, fmul, fdiv
		if funct5 == 0x01 {
			b = -b
		}
		r, e := arith(funct5, a, b)
		c.raise(arithFlags(funct5, a, b, r, isSNaND(ab) || isSNaND(bb)))
		c.setD(rd, c.roundD(r, e, rm, !math.IsInf(a, 0) && !math.IsInf(b, 0)))
	case 0x0b: // fsqrt
		if rs2 != 0 {
			return false
		}
		r, e := sqrtErr(a)
		if (a < 0 && !isNaND(ab)) || isSNaND(ab) {
			c.raise(flagNV)
		}
		c.setD(rd, c.roundD(r, e, rm, true))
	case 0x04: // fsgnj
		var s uint64
		switch f3 {
		case 0:
			s = bb
		case 1:
			s = ^bb
		case 2:
			s = ab ^ bb
		default:
			return false
		}
		c.setD(rd, ab&^(1<<63)|s&(1<<63))
	case 0x05: // fmin, fmax
		if f3 > 1 {
			return false
		}
		if isSNaND(ab) || isSNaND(bb) {
			c.raise(flagNV)
		}
		c.setD(rd, minmaxD(ab, bb, f3 == 1))
	case 0x08: // fcvt.d.s
		if rs2 != 0 {
			return false
		}
		s := c.getS(rs1)
		if isNaNS(s) {
			if isSNaNS(s) {
				c.raise(flagNV)
			}
			c.setD(rd, qnanD)
		} else {
			c.setD(rd, math.Float64bits(float64(math.Float32frombits(s))))
		}
	case 0x14: // feq, flt, fle
		c.X[rd] = c.compare(f3, isNaND(ab), isNaND(bb), isSNaND(ab) || isSNaND(bb), a, b)
		return f3 <= 2
	case 0x18: // fcvt.{w,wu,l,lu}.d
		if rs2 > 3 {
			return false
		}
		c.X[rd] = c.toInt(a, isNaND(ab), rs2, rm)
	case 0x1a: // fcvt.d.{w,wu,l,lu}
		if rs2 > 3 {
			return false
		}
		r, e := intToFloat(c.X[rs1], rs2, false)
		c.setD(rd, c.roundD(r, e, rm, true))
	case 0x1c:
		switch f3 {
		case 0: // fmv.x.d
			c.X[rd] = ab
		case 1:
			c.X[rd] = classD(ab)
		default:
			return false
		}
	case 0x1e: // fmv.d.x
		if f3 != 0 {
			return false
		}
		c.setD(rd, c.X[rs1])
	default:
		return false
	}
	return true
}

// arith computes a op b in double with the sign of the rounding error
// (exact - result).
func arith(funct5 uint32, a, b float64) (float64, int) {
	switch funct5 {
	case 0x00, 0x01:
		s := a + b
		if math.IsInf(s, 0) || math.IsNaN(s) {
			return s, 0
		}
		bv := s - a
		err := (a - (s - bv)) + (b - bv)
		return s, sign(err)
	case 0x02:
		p := a * b
		if math.IsInf(p, 0) || math.IsNaN(p) {
			return p, 0
		}
		return p, sign(math.FMA(a, b, -p))
	default:
		q := a / b
		if math.IsInf(q, 0) || math.IsNaN(q) || q == 0 && a == 0 {
			return q, 0
		}
		if math.IsInf(b, 0) {
			return q, 0 // finite / inf is an exact zero
		}
		res := math.FMA(-q, b, a)
		return q, sign(res) * sign(b)
	}
}

func sqrtErr(a float64) (float64, int) {
	r := math.Sqrt(a)
	if math.IsNaN(r) || math.IsInf(r, 0) || r == 0 {
		return r, 0
	}
	return r, sign(math.FMA(-r, r, a))
}

func arithFlags(funct5 uint32, a, b, r float64, snan bool) uint64 {
	var f uint64
	if snan || (math.IsNaN(r) && !math.IsNaN(a) && !math.IsNaN(b)) {
		f |= flagNV
	}
	if funct5 == 0x03 && b == 0 && !math.IsNaN(a) && !math.IsInf(a, 0) && a != 0 {
		f |= flagDZ
	}
	return f
}

// roundD applies the rounding mode and raises NX/OF/UF for a double result r
// whose exact value differs from r in the direction e.
func (c *CPU) roundD(r float64, e int, rm uint64, finiteIn bool) uint64 {
	if math.IsNaN(r) {
		return qnanD
	}
	if math.IsInf(r, 0) {
		if finiteIn {
			c.raise(flagOF | flagNX)
			switch {
			case rm == rmRTZ, rm == rmRDN && r > 0, rm == rmRUP && r < 0:
				return math.Float64bits(math.Copysign(math.MaxFloat64, r))
			}
		}
		return math.Float64bits(r)
	}
	r = directed(r, e, rm)
	if e != 0 {
		c.raise(flagNX)
		if math.Abs(r) < 0x1p-1022 {
			c.raise(flagUF)
		}
	}
	return math.Float64bits(r)
}

func directed(r float64, e int, rm uint64) float64 {
	switch {
	case e == 0:
	case rm == rmRTZ:
		if e < 0 && r > 0 || e > 0 && r < 0 {
			r = math.Nextafter(r, 0)
		}
	case rm == rmRDN && e < 0:
		r = math.Nextafter(r, math.Inf(-1))
	case rm == rmRUP && e > 0:
		r = math.Nextafter(r, math.Inf(1))
	}
	return r
}

func (c *CPU) fmaD(ab, bb, db uint64, negProd, negAdd bool, rm uint64) uint64 {
	a, b, d := math.Float64frombits(ab), math.Float64frombits(bb), math.Float64frombits(db)
	if negProd {
		a = -a
	}
	if negAdd {
		d = -d
	}
	r := math.FMA(a, b, d)
	if isSNaND(ab) || isSNaND(bb) || isSNaND(db) ||
		(math.IsInf(a, 0) && b == 0 || a == 0 && math.IsInf(b, 0)) ||
		math.IsNaN(r) && !math.IsNaN(a) && !math.IsNaN(b) && !math.IsNaN(d) {
		c.raise(flagNV)
	}
	finite := !math.IsInf(a, 0) && !math.IsInf(b, 0) && !math.IsInf(d, 0)
	return c.roundD(r, fmaErr(a, b, d, r), rm, finite)
}

// fmaErr returns sign(a*b+d - r) computed exactly with big.Float. FMA is rare
// enough that the allocation does not matter.
func fmaErr(a, b, d, r float64) int {
	if math.IsNaN(r) || math.IsInf(r, 0) || math.IsInf(a, 0) || math.IsInf(b, 0) || math.IsInf(d, 0) {
		return 0
	}
	x := new(big.Float).SetPrec(2200).SetFloat64(a)
	x.Mul(x, new(big.Float).SetFloat64(b))
	x.Add(x, new(big.Float).SetFloat64(d))
	x.Sub(x, new(big.Float).SetFloat64(r))
	return x.Sign()
}

func minmaxD(ab, bb uint64, max bool) uint64 {
	an, bn := isNaND(ab), isNaND(bb)
	switch {
	case an && bn:
		return qnanD
	case an:
		return bb
	case bn:
		return ab
	}
	a, b := math.Float64frombits(ab), math.Float64frombits(bb)
	if a == b { // handles -0 vs +0
		if max == (ab>>63 == 0) {
			return ab
		}
		return bb
	}
	if (a > b) == max {
		return ab
	}
	return bb
}

func classD(b uint64) uint64 {
	neg := b>>63 != 0
	exp := b >> 52 & 0x7ff
	frac := b & (1<<52 - 1)
	return class(neg, exp == 0x7ff, exp == 0, frac == 0, b>>51&1 == 1)
}

func class(neg, expMax, expZero, fracZero, quiet bool) uint64 {
	switch {
	case expMax && fracZero:
		return pick(neg, 1<<0, 1<<7)
	case expMax:
		if quiet {
			return 1 << 9
		}
		return 1 << 8
	case expZero && fracZero:
		return pick(neg, 1<<3, 1<<4)
	case expZero:
		return pick(neg, 1<<2, 1<<5)
	}
	return pick(neg, 1<<1, 1<<6)
}

func pick(neg bool, n, p uint64) uint64 {
	if neg {
		return n
	}
	return p
}

func (c *CPU) compare(f3 uint32, aNaN, bNaN, snan bool, a, b float64) uint64 {
	if aNaN || bNaN {
		if snan || f3 != 2 {
			c.raise(flagNV)
		}
		return 0
	}
	switch f3 {
	case 0:
		return b2u(a <= b)
	case 1:
		return b2u(a < b)
	default:
		return b2u(a == b)
	}
}

// toInt converts x to an integer of the kind selected by typ (0 w, 1 wu,
// 2 l, 3 lu) with rounding mode rm, saturating and raising NV when out of
// range.
func (c *CPU) toInt(x float64, nan bool, typ uint32, rm uint64) uint64 {
	var lo, hi float64 // inclusive valid range of the rounded value
	var minV, maxV uint64
	switch typ {
	case 0:
		lo, hi, minV, maxV = math.MinInt32, math.MaxInt32, sext32(1<<31), 1<<31-1
	case 1:
		lo, hi, minV, maxV = 0, math.MaxUint32, 0, ^uint64(0) // wu saturates to sext(0xffffffff)
	case 2:
		lo, hi, minV, maxV = -0x1p63, 0x1p63, 1<<63, 1<<63-1
	default:
		lo, hi, minV, maxV = 0, 0x1p64, 0, ^uint64(0)
	}
	if nan {
		c.raise(flagNV)
		return maxV
	}
	v := roundInt(x, rm)
	// For 64-bit types hi is 2^63/2^64, which is itself out of range.
	if v < lo || v > hi || (typ >= 2 && v == hi) {
		c.raise(flagNV)
		if v < lo {
			return minV
		}
		return maxV
	}
	if v != x {
		c.raise(flagNX)
	}
	switch typ {
	case 0:
		return uint64(int64(v))
	case 1:
		return sext32(uint64(uint32(v)))
	case 2:
		return uint64(int64(v))
	default:
		return uint64(v)
	}
}

func roundInt(x float64, rm uint64) float64 {
	switch rm {
	case rmRTZ:
		return math.Trunc(x)
	case rmRDN:
		return math.Floor(x)
	case rmRUP:
		return math.Ceil(x)
	case rmRMM:
		return math.Round(x)
	}
	return math.RoundToEven(x)
}

// intToFloat converts the integer in v (typ: 0 w, 1 wu, 2 l, 3 lu) to double
// or, if single, to a single-precision value held in a double. It returns the
// rounding error sign. Converting straight to the target precision avoids
// double rounding.
func intToFloat(v uint64, typ uint32, single bool) (float64, int) {
	if typ == 0 || typ == 2 {
		x := int64(v)
		if typ == 0 {
			x = int64(int32(v))
		}
		r := float64(x)
		if single {
			r = float64(float32(x))
		}
		if r >= 0x1p63 {
			return r, -1
		}
		return r, cmpInt(x, int64(r))
	}
	u := v
	if typ == 1 {
		u = uint64(uint32(v))
	}
	r := float64(u)
	if single {
		r = float64(float32(u))
	}
	if r >= 0x1p64 {
		return r, -1
	}
	return r, cmpUint(u, uint64(r))
}

func cmpInt(a, b int64) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	}
	return 0
}

func cmpUint(a, b uint64) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	}
	return 0
}

// --- single precision ---

func (c *CPU) fpS(funct5, rd, rs1, rs2, f3 uint32, rm uint64) bool {
	ab, bb := c.getS(rs1), c.getS(rs2)
	a := float64(math.Float32frombits(ab))
	b := float64(math.Float32frombits(bb))
	switch funct5 {
	case 0x00, 0x01, 0x02, 0x03:
		if funct5 == 0x01 {
			b = -b
		}
		r, e := arith(funct5, a, b)
		c.raise(arithFlags(funct5, a, b, r, isSNaNS(ab) || isSNaNS(bb)))
		c.setS(rd, c.roundS(r, e, rm, !math.IsInf(a, 0) && !math.IsInf(b, 0)))
	case 0x0b:
		if rs2 != 0 {
			return false
		}
		r, e := sqrtErr(a)
		if (a < 0 && !isNaNS(ab)) || isSNaNS(ab) {
			c.raise(flagNV)
		}
		c.setS(rd, c.roundS(r, e, rm, true))
	case 0x04:
		var s uint32
		switch f3 {
		case 0:
			s = bb
		case 1:
			s = ^bb
		case 2:
			s = ab ^ bb
		default:
			return false
		}
		c.setS(rd, ab&^(1<<31)|s&(1<<31))
	case 0x05:
		if f3 > 1 {
			return false
		}
		if isSNaNS(ab) || isSNaNS(bb) {
			c.raise(flagNV)
		}
		c.setS(rd, minmaxS(ab, bb, f3 == 1))
	case 0x08: // fcvt.s.d
		if rs2 != 1 {
			return false
		}
		db := c.F[rs1]
		if isNaND(db) {
			if isSNaND(db) {
				c.raise(flagNV)
			}
			c.setS(rd, qnanS)
		} else {
			d := math.Float64frombits(db)
			c.setS(rd, c.roundS(d, 0, rm, !math.IsInf(d, 0)))
		}
	case 0x14:
		c.X[rd] = c.compare(f3, isNaNS(ab), isNaNS(bb), isSNaNS(ab) || isSNaNS(bb), a, b)
		return f3 <= 2
	case 0x18:
		if rs2 > 3 {
			return false
		}
		c.X[rd] = c.toInt(a, isNaNS(ab), rs2, rm)
	case 0x1a:
		if rs2 > 3 {
			return false
		}
		r, e := intToFloat(c.X[rs1], rs2, true)
		c.setS(rd, c.roundS(r, e, rm, true))
	case 0x1c:
		switch f3 {
		case 0: // fmv.x.w
			c.X[rd] = sext32(c.F[rs1])
		case 1:
			c.X[rd] = classS(ab)
		default:
			return false
		}
	case 0x1e: // fmv.w.x
		if f3 != 0 {
			return false
		}
		c.setS(rd, uint32(c.X[rs1]))
	default:
		return false
	}
	return true
}

// roundS rounds the double r (whose exact value differs from it in the
// direction e) to single precision under rm.
func (c *CPU) roundS(r float64, e int, rm uint64, finiteIn bool) uint32 {
	if math.IsNaN(r) {
		return qnanS
	}
	s := float32(r)
	if math.IsInf(float64(s), 0) {
		if finiteIn {
			c.raise(flagOF | flagNX)
			switch {
			case rm == rmRTZ, rm == rmRDN && r > 0, rm == rmRUP && r < 0:
				return math.Float32bits(float32(math.Copysign(math.MaxFloat32, r)))
			}
		}
		return math.Float32bits(s)
	}
	if d := float64(s); d != r {
		e = sign(r - d)
	}
	if e != 0 {
		switch {
		case rm == rmRTZ && (e < 0 && s > 0 || e > 0 && s < 0):
			s = math.Nextafter32(s, 0)
		case rm == rmRDN && e < 0:
			s = math.Nextafter32(s, float32(math.Inf(-1)))
		case rm == rmRUP && e > 0:
			s = math.Nextafter32(s, float32(math.Inf(1)))
		}
		c.raise(flagNX)
		if math.Abs(float64(s)) < 0x1p-126 {
			c.raise(flagUF)
		}
	}
	return math.Float32bits(s)
}

func (c *CPU) fmaS(ab, bb, db uint32, negProd, negAdd bool, rm uint64) uint32 {
	a := float64(math.Float32frombits(ab))
	b := float64(math.Float32frombits(bb))
	d := float64(math.Float32frombits(db))
	if negProd {
		a = -a
	}
	if negAdd {
		d = -d
	}
	r := math.FMA(a, b, d) // a*b is exact in double, so this is one rounding
	if isSNaNS(ab) || isSNaNS(bb) || isSNaNS(db) ||
		(math.IsInf(a, 0) && b == 0 || a == 0 && math.IsInf(b, 0)) ||
		math.IsNaN(r) && !math.IsNaN(a) && !math.IsNaN(b) && !math.IsNaN(d) {
		c.raise(flagNV)
	}
	finite := !math.IsInf(a, 0) && !math.IsInf(b, 0) && !math.IsInf(d, 0)
	return c.roundS(r, fmaErr(a, b, d, r), rm, finite)
}

func minmaxS(ab, bb uint32, max bool) uint32 {
	an, bn := isNaNS(ab), isNaNS(bb)
	switch {
	case an && bn:
		return qnanS
	case an:
		return bb
	case bn:
		return ab
	}
	a, b := math.Float32frombits(ab), math.Float32frombits(bb)
	if a == b {
		if max == (ab>>31 == 0) {
			return ab
		}
		return bb
	}
	if (a > b) == max {
		return ab
	}
	return bb
}

func classS(b uint32) uint64 {
	exp := b >> 23 & 0xff
	frac := b & (1<<23 - 1)
	return class(b>>31 != 0, exp == 0xff, exp == 0, frac == 0, b>>22&1 == 1)
}
