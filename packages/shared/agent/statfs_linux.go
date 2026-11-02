package agent

import "syscall"

func statfs(path string) (FS, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return FS{}, err
	}
	bs := uint64(s.Bsize)
	return FS{Mount: path, Total: s.Blocks * bs, Free: s.Bavail * bs, Used: (s.Blocks - s.Bfree) * bs}, nil
}
