//go:build unix

package cmd

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in its own session, so it survives the hook process and
// isn't tied to the agent's terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
