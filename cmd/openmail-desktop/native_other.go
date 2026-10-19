//go:build !darwin

package main

func setBadge(int)              {}
func appIsActive() bool         { return false }
func notify(title, body string) {}
