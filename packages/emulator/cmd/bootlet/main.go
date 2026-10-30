// Command bootlet boots a RISC-V Linux kernel natively in the terminal, using
// the same machine package as the browser build. Press Ctrl-A x to quit.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/term"

	"bootlet/emulator/devices"
	"bootlet/emulator/machine"
	"bootlet/emulator/netstack"
)

func main() {
	kernel := flag.String("kernel", "", "raw RISC-V Linux Image")
	initrd := flag.String("initrd", "", "initramfs (cpio)")
	mem := flag.Uint64("mem", machine.DefaultRAM>>20, "RAM size in MiB")
	cmdline := flag.String("cmdline", machine.DefaultCmdline, "kernel command line")
	quiet := flag.Bool("quiet", false, "hide the kernel boot log")
	diskPath := flag.String("disk", "", "persistent root disk image (created if missing)")
	diskSize := flag.Uint64("disk-size", 1024, "size in MiB of a newly created disk (sparse)")
	direct := flag.Bool("net", false, "give the guest network access through this process's sockets")
	relayURL := flag.String("relay", "", "give the guest network access through a Wisp relay (ws:// or wss:// URL)")
	flag.Parse()
	log.SetFlags(0)

	if *quiet {
		*cmdline += " " + machine.QuietCmdline
	}
	m := machine.New(machine.Config{RAMSize: *mem << 20, Cmdline: *cmdline})
	data, err := os.ReadFile(*kernel)
	if err != nil {
		log.Fatal(err)
	}
	if err := m.LoadKernel(data); err != nil {
		log.Fatal(err)
	}
	if *initrd != "" {
		data, err := os.ReadFile(*initrd)
		if err != nil {
			log.Fatal(err)
		}
		m.LoadInitrd(data)
	}
	var saveDisk func()
	if *diskPath != "" {
		disk, save, err := openDisk(*diskPath, *diskSize<<20)
		if err != nil {
			log.Fatal(err)
		}
		m.AttachDisk(disk)
		saveDisk = save
		defer save()
	}
	// Networking: netWake signals upstream events while the guest idles,
	// netPump moves relay packets (a no-op for direct sockets).
	var netWake <-chan struct{}
	netPump := func() {}
	switch {
	case *relayURL != "":
		up := netstack.NewWispUpstream()
		link, err := dialRelay(*relayURL, up)
		if err != nil {
			log.Fatalf("relay: %v", err)
		}
		m.AttachNet(netstack.New(up))
		netWake, netPump = link.wake, link.pump
	case *direct:
		up := netstack.NewDirectUpstream()
		m.AttachNet(netstack.New(up))
		netWake = up.Notify
	}
	if err := m.Reset(); err != nil {
		log.Fatal(err)
	}

	input := make(chan []byte, 16)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		old, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			log.Fatal(err)
		}
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	go func() {
		for {
			buf := make([]byte, 256)
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(input)
				return
			}
			input <- buf[:n]
		}
	}()

	start := time.Now()
	var instret uint64
	escape := false
	feed := func(b []byte) bool {
		for _, c := range b {
			switch {
			case escape && c == 'x':
				return false
			case c == 0x01 && !escape:
				escape = true
				continue
			}
			escape = false
			m.Input([]byte{c})
		}
		return true
	}

	// recv handles one read from stdin; it returns false to quit.
	recv := func(b []byte, ok bool) bool {
		if !ok {
			input = nil // stdin closed: keep running without input
			return true
		}
		return feed(b)
	}

loop:
	for {
		netPump()
		res := m.Run(50 * time.Millisecond)
		instret += res.Instructions
		os.Stdout.Write(m.Output())
		netPump()
		if saveDisk != nil {
			saveDisk()
		}
		if res.Halted {
			break
		}
		for drained := false; !drained; {
			select {
			case b, ok := <-input:
				if !recv(b, ok) {
					break loop
				}
			default:
				drained = true
			}
		}
		if res.Idle {
			// Sleep until input or network data arrives or the next timer
			// event is due.
			var wait <-chan time.Time
			if res.IdleFor >= 0 {
				wait = time.After(res.IdleFor)
			}
			select {
			case b, ok := <-input:
				if !recv(b, ok) {
					break loop
				}
			case <-netWake:
			case <-wait:
			}
		}
	}
	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "\r\n[bootlet] %d instructions in %v (%.1f MIPS)\r\n",
		instret, elapsed.Round(time.Millisecond), float64(instret)/elapsed.Seconds()/1e6)
}

// openDisk loads a sparse disk image file (or formats a new one) and returns
// a function that writes changed blocks back to the file.
func openDisk(path string, size uint64) (*devices.Disk, func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if st.Size() > 0 {
		size = uint64(st.Size())
	}
	disk := devices.NewDisk(size)
	buf := make([]byte, devices.BlockSize)
	for i := uint64(0); i*devices.BlockSize < uint64(st.Size()); i++ {
		if _, err := f.ReadAt(buf, int64(i*devices.BlockSize)); err != nil {
			return nil, nil, err
		}
		for _, b := range buf {
			if b != 0 {
				disk.Restore(i, buf)
				break
			}
		}
	}
	if st.Size() == 0 {
		disk.Format("bootlet")
		if err := f.Truncate(int64(size)); err != nil {
			return nil, nil, err
		}
	}
	save := func() {
		idx, data := disk.TakeDirty()
		for i, n := range idx {
			if _, err := f.WriteAt(data[i], int64(n*devices.BlockSize)); err != nil {
				log.Printf("saving disk: %v", err)
			}
		}
	}
	return disk, save, nil
}
