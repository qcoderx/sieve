//go:build windows

package render

// syscallKillGroup is unused on Windows, where the tree is taken by taskkill.
func syscallKillGroup(pid int) error { return nil }
