//go:build boot

package machine_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
	"bootlet/emulator/netstack"
)

// TestDeterministic boots in deterministic mode twice with each execution
// engine: every run must reach the prompt after exactly the same number of
// instructions, with identical memory and registers. This compares the block
// engine with the interpreter over a whole Linux boot.
func TestDeterministic(t *testing.T) {
	disk := devices.NewDisk(256 << 20)
	disk.Format("test") // has a random UUID and the current time: share one
	run := func(engine string) (uint64, [32]byte, uint64) {
		g := newGuest(t, machine.Config{Deterministic: true, Display: true, Engine: engine}, func(m *machine.Machine) {
			m.ReplaceDisk(disk.Clone())
			// As in the browser: the keyboard, and networking with no relay.
			m.AttachKeyboard()
			m.AttachNet(netstack.New(netstack.NewWispUpstream()))
		})
		g.waitFor(prompt, 2*time.Minute)
		return g.instret, sha256.Sum256(g.m.RAM), g.m.CPU.PC
	}
	n1, h1, pc1 := run(machine.EngineInterp)
	t.Logf("prompt after %d instructions", n1)
	for _, engine := range []string{machine.EngineInterp, machine.EngineBlocks, machine.EngineBlocks} {
		n2, h2, pc2 := run(engine)
		if n1 != n2 || h1 != h2 || pc1 != pc2 {
			t.Fatalf("%s run differs from the interpreter: %d vs %d instructions, pc %x vs %x, memory equal %v",
				engine, n1, n2, pc1, pc2, h1 == h2)
		}
	}
}
