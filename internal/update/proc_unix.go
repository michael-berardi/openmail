//go:build unix

package update

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own session so Ctrl-C in the terminal does not
// stop it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// LowerPriority makes background work yield to the user's programs.
func LowerPriority() { _ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10) }
