//go:build !windows

package workspace

import (
	"context"
	"os/exec"
)

func shell(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}
