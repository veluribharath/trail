//go:build !unix

package main

import "os/exec"

func detach(cmd *exec.Cmd) {}
