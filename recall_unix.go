//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a session of its own, so it outlives the hook that
// started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
