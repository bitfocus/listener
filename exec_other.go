//go:build !windows

package main

import (
	"context"
	"os/exec"
)

func shellCommand(ctx context.Context, shell string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", shell)
}
