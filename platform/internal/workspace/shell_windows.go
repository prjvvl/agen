package workspace

import (
	"context"
	"os/exec"
	"syscall"
)

// shell runs a command line with cmd.exe as typed: Go's argument quoting
// would escape its quotes in a way cmd does not understand.
func shell(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /S /C "` + command + `"`}
	return cmd
}
