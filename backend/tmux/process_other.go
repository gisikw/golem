//go:build !linux

package tmux

import "os/exec"

func configureCommand(c *exec.Cmd) {}
