//go:build linux

package tmux

import (
	"os/exec"
	"syscall"
)

func configureCommand(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }
