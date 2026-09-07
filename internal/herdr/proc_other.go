//go:build !unix

package herdr

import "syscall"

// detachProcAttr: no setsid on this platform.
func detachProcAttr() *syscall.SysProcAttr {
	return nil
}
