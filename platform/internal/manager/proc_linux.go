package manager

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func prepareCmd(cmd *exec.Cmd) {
	// Hosts die with the Manager.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// afterStart caps the host's address space (RLIMIT_AS) when memLimit is
// set. It reports whether the limit is enforced.
func afterStart(cmd *exec.Cmd, memLimit uint64) bool {
	if memLimit == 0 {
		return true
	}
	lim := unix.Rlimit{Cur: memLimit, Max: memLimit}
	return unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_AS, &lim, nil) == nil
}

// terminate sends SIGTERM: the host drains running tasks, then exits.
func terminate(cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM) }
