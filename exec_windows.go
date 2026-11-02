//go:build windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs shell through cmd.exe with a hand-built command line, so Go's
// argv quoting never reaches cmd's own quote handling. The console stays hidden.
func shellCommand(ctx context.Context, shell string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:    windowsShellCmdLine(shell),
		HideWindow: true,
	}
	return cmd
}
