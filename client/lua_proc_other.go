//go:build !unix

package client

import "os/exec"

// killGroupOnCancel leaves cmd as exec.CommandContext made it where there
// are no process groups to kill: the cancellation of its context kills the
// process alone.
func killGroupOnCancel(*exec.Cmd) {}
