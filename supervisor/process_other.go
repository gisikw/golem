//go:build !linux

package supervisor

import "os/exec"

func configureCommand(c *exec.Cmd) {}
