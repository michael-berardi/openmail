//go:build !unix

package core

// acquireEngineLock is a no-op where advisory locks are unavailable.
func (a *App) acquireEngineLock() (release func(), ok bool, err error) {
	return func() {}, true, nil
}
