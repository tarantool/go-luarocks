//go:build !unix

package build

import "os/exec"

// killGroupOnCancel leaves cmd as exec.CommandContext made it where there
// are no process groups to kill: the cancellation of its context kills the
// process alone, and cmd.WaitDelay bounds the wait for its output.
func killGroupOnCancel(*exec.Cmd) {}
