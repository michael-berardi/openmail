//go:build !unix

package update

import "os/exec"

func detach(*exec.Cmd) {}

// LowerPriority is a no-op where the platform has no portable call for it.
func LowerPriority() {}
