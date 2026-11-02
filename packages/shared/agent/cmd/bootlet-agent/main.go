// Command bootlet-agent runs in the guest and serves the page's file
// explorer over the virtio console port named "bootlet.agent". init starts
// it (and restarts it if it exits); messages go to the kernel log (dmesg).
package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bootlet/shared/agent"
)

const portName = "bootlet.agent"

func main() {
	log.SetFlags(0)
	log.SetPrefix("<6>bootlet-agent: ") // KERN_INFO: dmesg only, never the console
	if kmsg, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); err == nil {
		log.SetOutput(kmsg)
	} else {
		log.SetOutput(io.Discard) // stderr is the serial console
	}
	for {
		dev, err := port()
		if err == nil {
			var f *os.File
			if f, err = os.OpenFile(dev, os.O_RDWR, 0); err == nil {
				log.Printf("serving on %s", dev)
				err = agent.Serve(f)
				f.Close()
			}
		}
		log.Printf("%v; retrying", err)
		time.Sleep(2 * time.Second)
	}
}

// port returns the device node of the agent's console port.
func port() (string, error) {
	names, _ := filepath.Glob("/sys/class/virtio-ports/*/name")
	for _, n := range names {
		if b, err := os.ReadFile(n); err == nil && strings.TrimSpace(string(b)) == portName {
			return "/dev/" + filepath.Base(filepath.Dir(n)), nil
		}
	}
	return "", os.ErrNotExist
}
