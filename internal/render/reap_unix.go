//go:build !windows

package render

import "syscall"

// syscallKillGroup kills the browser's whole process group.
func syscallKillGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
