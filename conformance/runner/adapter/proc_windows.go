//go:build windows

package adapter

import (
	"os"
	"syscall"
)

func setpgid() *syscall.SysProcAttr { return nil }

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
