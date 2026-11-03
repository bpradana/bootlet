//go:build boot

package machine_test

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
)

// benchResult is one measured workload: wall-clock time, guest instructions,
// and the time spent inside Machine.Run (active), which excludes the host
// sleeping while the guest idles.
type benchResult struct {
	Name         string  `json:"name"`
	WallMs       float64 `json:"wallMs"`
	ActiveMs     float64 `json:"activeMs"`
	Instructions uint64  `json:"instructions"`
	ActiveMIPS   float64 `json:"activeMips"`
	WallMIPS     float64 `json:"wallMips"`
}

func measure(name string, g *guest, f func()) benchResult {
	i0, b0, t0 := g.instret, g.busy, time.Now()
	f()
	wall, busy, n := time.Since(t0), g.busy-b0, g.instret-i0
	return benchResult{name, ms(wall), ms(busy), n, float64(n) / busy.Seconds() / 1e6, float64(n) / wall.Seconds() / 1e6}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Guest workloads: fixed amounts of work, run from the shell.
var benchWorkloads = []struct{ name, cmd string }{
	{"md5sum-16MiB", "dd if=/dev/zero bs=1M count=16 2>/dev/null | md5sum"},
	{"gzip-2MiB", "head -c 2097152 /dev/zero | tr '\\000' 'a' | gzip -1 | wc -c"},
	{"shell-loop-20k", "i=0; while [ $i -lt 20000 ]; do i=$((i+1)); done; echo $i"},
}

// TestBench measures boot and fixed CPU workloads natively; the browser
// counterpart is apps/frontend/bench. It only runs with BOOTLET_BENCH=1:
//
//	BOOTLET_BENCH=1 [BOOTLET_BENCH_RUNS=5] [BOOTLET_BENCH_OUT=file.json] [BOOTLET_ENGINE=interp] \
//	  go test -tags boot -run TestBench -v ./machine [-cpuprofile cpu.out]
//
// "deterministic-boot" boots with the instruction-count clock: exactly the
// same instructions every run, so its MIPS is the cleanest speed figure.
func TestBench(t *testing.T) {
	if os.Getenv("BOOTLET_BENCH") == "" {
		t.Skip("set BOOTLET_BENCH=1 to run the benchmarks")
	}
	runs := 3
	if n, err := strconv.Atoi(os.Getenv("BOOTLET_BENCH_RUNS")); err == nil && n > 0 {
		runs = n
	}
	engine := os.Getenv("BOOTLET_ENGINE") // machine.EngineBlocks if empty
	disk := devices.NewDisk(256 << 20)
	disk.Format("bench")

	all := map[string][]benchResult{}
	for r := 0; r < runs; r++ {
		g := newGuest(t, machine.Config{Deterministic: true, Display: true, Engine: engine}, func(m *machine.Machine) { m.ReplaceDisk(disk.Clone()) })
		all["deterministic-boot"] = append(all["deterministic-boot"], measure("deterministic-boot", g, func() { g.waitFor(prompt, 5*time.Minute) }))

		g = newGuest(t, machine.Config{Display: true, Splash: true, Engine: engine}, func(m *machine.Machine) {
			m.ReplaceDisk(disk.Clone())
			m.AttachKeyboard()
		})
		all["boot"] = append(all["boot"], measure("boot", g, func() { g.waitFor(prompt, 5*time.Minute) }))
		for _, w := range benchWorkloads {
			all[w.name] = append(all[w.name], measure(w.name, g, func() { g.sh(w.cmd, 10*time.Minute) }))
		}
	}

	var summary []benchResult
	for _, name := range append([]string{"deterministic-boot", "boot"}, workloadNames()...) {
		rs := all[name]
		med := func(f func(benchResult) float64) float64 {
			v := make([]float64, len(rs))
			for i, r := range rs {
				v[i] = f(r)
			}
			slices.Sort(v)
			return v[len(v)/2]
		}
		m := benchResult{
			Name:         name,
			WallMs:       med(func(r benchResult) float64 { return r.WallMs }),
			ActiveMs:     med(func(r benchResult) float64 { return r.ActiveMs }),
			Instructions: uint64(med(func(r benchResult) float64 { return float64(r.Instructions) })),
			ActiveMIPS:   med(func(r benchResult) float64 { return r.ActiveMIPS }),
			WallMIPS:     med(func(r benchResult) float64 { return r.WallMIPS }),
		}
		lo, hi := rs[0].ActiveMIPS, rs[0].ActiveMIPS
		for _, r := range rs {
			lo, hi = min(lo, r.ActiveMIPS), max(hi, r.ActiveMIPS)
		}
		t.Logf("%-20s wall %8.0f ms  active %8.0f ms  %11d instr  active %6.1f MIPS (%.1f-%.1f)  wall %6.1f MIPS",
			name, m.WallMs, m.ActiveMs, m.Instructions, m.ActiveMIPS, lo, hi, m.WallMIPS)
		summary = append(summary, m)
	}
	if out := os.Getenv("BOOTLET_BENCH_OUT"); out != "" {
		b, _ := json.MarshalIndent(map[string]any{"runs": runs, "engine": os.Getenv("BOOTLET_ENGINE"), "results": summary, "all": all}, "", "  ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func workloadNames() []string {
	var n []string
	for _, w := range benchWorkloads {
		n = append(n, w.name)
	}
	return n
}
