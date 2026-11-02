//go:build !linux

package agent

import "errors"

// The agent only runs in the Linux guest; elsewhere (tests on the host)
// filesystems are not reported.
func statfs(string) (FS, error) { return FS{}, errors.ErrUnsupported }
