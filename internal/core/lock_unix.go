//go:build unix

package core

import (
	"os"
	"path/filepath"
	"syscall"
)

// acquireEngineLock takes an exclusive advisory lock so only one process syncs at a time.
// The lock is released when the process exits.
func (a *App) acquireEngineLock() (release func(), ok bool, err error) {
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(a.Dir, "engine.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true, nil
}
