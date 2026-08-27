//go:build !windows

package adapter

import "syscall"

// setpgid puts the adapter in its own process group, so killGroup can take
// down every descendant it spawns (docs/CONFORMANCE.md section 5's "kill
// process group" requirement) instead of leaving orphans behind.
func setpgid() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(pid int) {
	// Negative pid signals the whole process group (see setpgid above).
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
