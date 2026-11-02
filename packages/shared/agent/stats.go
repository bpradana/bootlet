package agent

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Stats is a snapshot of the guest for the System tab. CPU, network and
// per-process times are cumulative counters: the page computes rates from
// the difference between two snapshots.
type Stats struct {
	Uptime float64    `json:"uptime"` // seconds since boot
	Load   [3]float64 `json:"load"`
	Ticks  int        `json:"ticks"` // clock ticks per second (USER_HZ) of CPU and process times
	CPU    CPUTimes   `json:"cpu"`
	Mem    Mem        `json:"mem"`
	FS     []FS       `json:"fs"`
	Net    *Net       `json:"net,omitempty"` // eth0, when present
	Tasks  int        `json:"tasks"`         // processes and kernel threads
	Procs  []Proc     `json:"procs"`         // processes, without kernel threads
}

// CPUTimes are cumulative ticks: User includes nice, System includes
// interrupt handling.
type CPUTimes struct {
	User   uint64 `json:"user"`
	System uint64 `json:"system"`
	IOWait uint64 `json:"iowait"`
	Idle   uint64 `json:"idle"`
}

// Mem is in bytes, from /proc/meminfo.
type Mem struct {
	Total     uint64 `json:"total"`
	Free      uint64 `json:"free"`
	Available uint64 `json:"available"`
}

// FS is a mounted filesystem's size in bytes.
type FS struct {
	Mount string `json:"mount"`
	Type  string `json:"type"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"` // available to unprivileged users
	Used  uint64 `json:"used"`
}

// Net holds cumulative byte counts.
type Net struct {
	RX uint64 `json:"rx"`
	TX uint64 `json:"tx"`
}

// Proc is a process: cumulative CPU ticks and resident memory in bytes.
type Proc struct {
	PID   int    `json:"pid"`
	Name  string `json:"name"`
	Ticks uint64 `json:"ticks"`
	RSS   uint64 `json:"rss"`
}

// statMounts are the filesystems reported, in order: the root (the
// persistent disk when it is an overlay) and the RAM-backed /tmp.
var statMounts = []string{"/", "/tmp"}

// ReadStats reads the snapshot from /proc. It only reads files, so a poll
// costs the guest little.
func ReadStats() (Stats, error) {
	st := Stats{Ticks: 100} // USER_HZ is 100 on every Linux architecture Go supports here
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return st, err
	}
	st.CPU = parseCPU(b)
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			st.Uptime, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		for i := 0; i < 3 && i < len(f); i++ {
			st.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		st.Mem = parseMeminfo(b)
	}
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		st.Net = parseNetDev(b, "eth0")
	}
	types := mountTypes()
	for _, m := range statMounts {
		if fs, err := statfs(m); err == nil && fs.Total > 0 {
			fs.Type = types[m]
			st.FS = append(st.FS, fs)
		}
	}
	st.Procs, st.Tasks = readProcs()
	return st, nil
}

func parseCPU(b []byte) CPUTimes {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	n := func(i int) uint64 {
		if i >= len(f) {
			return 0
		}
		v, _ := strconv.ParseUint(f[i], 10, 64)
		return v
	}
	// cpu user nice system idle iowait irq softirq steal
	return CPUTimes{User: n(1) + n(2), System: n(3) + n(6) + n(7) + n(8), Idle: n(4), IOWait: n(5)}
}

func parseMeminfo(b []byte) Mem {
	var m Mem
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		v *= 1024 // kB
		switch f[0] {
		case "MemTotal:":
			m.Total = v
		case "MemFree:":
			m.Free = v
		case "MemAvailable:":
			m.Available = v
		}
	}
	return m
}

func parseNetDev(b []byte, iface string) *Net {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) != iface {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return nil
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		return &Net{RX: rx, TX: tx}
	}
	return nil
}

// mountTypes maps mount points to filesystem types (the last mount wins).
func mountTypes() map[string]string {
	types := map[string]string{}
	b, _ := os.ReadFile("/proc/mounts")
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 3 {
			types[f[1]] = f[2]
		}
	}
	return types
}

// kthreads remembers the PIDs of kernel threads, which are most of the
// guest's tasks: generating /proc/<pid>/stat costs the guest about 90k
// instructions, so they are read once and skipped from then on.
var kthreads = map[int]bool{}

const pfKthread = 0x00200000 // PF_KTHREAD in the flags field

func readProcs() (procs []Proc, tasks int) {
	d, err := os.Open("/proc")
	if err != nil {
		return nil, 0
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	seen := map[int]bool{}
	buf := make([]byte, 1024)
	for _, n := range names {
		pid, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		tasks++
		seen[pid] = true
		if kthreads[pid] {
			continue
		}
		b, err := readSmall("/proc/"+n+"/stat", buf)
		if err != nil {
			continue // exited
		}
		p, flags, err := parseProcStat(b)
		if err != nil {
			continue
		}
		if flags&pfKthread != 0 {
			kthreads[pid] = true
			continue
		}
		procs = append(procs, p)
	}
	for pid := range kthreads {
		if !seen[pid] {
			delete(kthreads, pid) // the PID may be reused by a process
		}
	}
	return procs, tasks
}

// readSmall reads a small file with plain system calls: os.ReadFile also
// stats the file and tries to register it with the poller, which roughly
// doubles the cost for /proc files.
func readSmall(path string, buf []byte) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	n, err := syscall.Read(fd, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// parseProcStat reads /proc/<pid>/stat. The name is in parentheses and may
// contain spaces or parentheses itself, so fields are counted after the last
// ")". It also returns the task's flags.
func parseProcStat(b []byte) (Proc, uint64, error) {
	s := string(b)
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return Proc{}, 0, errors.New("malformed stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s[:open]))
	if err != nil {
		return Proc{}, 0, err
	}
	f := strings.Fields(s[end+1:])
	// f[0] is field 3 (state): flags is field 9, utime 14, stime 15, rss (pages) 24.
	if len(f) < 22 {
		return Proc{}, 0, errors.New("short stat")
	}
	flags, _ := strconv.ParseUint(f[6], 10, 64)
	utime, _ := strconv.ParseUint(f[11], 10, 64)
	stime, _ := strconv.ParseUint(f[12], 10, 64)
	rss, _ := strconv.ParseUint(f[21], 10, 64)
	return Proc{PID: pid, Name: s[open+1 : end], Ticks: utime + stime, RSS: rss * uint64(os.Getpagesize())}, flags, nil
}
