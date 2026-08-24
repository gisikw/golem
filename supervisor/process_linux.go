//go:build linux

package supervisor

import (
	"os/exec"
	"syscall"
)

func configureCommand(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }
