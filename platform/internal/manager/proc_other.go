//go:build !windows && !linux

package manager

import (
	"os/exec"
	"syscall"
)

func prepareCmd(*exec.Cmd) {}

// afterStart cannot cap memory on this platform: it reports false when a
// limit was asked for.
func afterStart(_ *exec.Cmd, memLimit uint64) bool { return memLimit == 0 }

func terminate(cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM) }
