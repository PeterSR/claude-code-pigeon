//go:build windows

package pigeon

import (
	"fmt"
	"io"
	"syscall"
	"time"
)

// MonitorSupported is false on Windows: Claude Code arms plugin monitors only
// on Unix, so the receiving half of pigeon cannot work here. The CLI still
// builds and can send to sessions on a Unix host sharing the state directory.
const MonitorSupported = false

const errSharingViolation = syscall.Errno(32)

type winLock struct{ h syscall.Handle }

func (l *winLock) Close() error { return syscall.CloseHandle(l.h) }

// openExclusive maps Unix's flock onto Windows share modes: opening with a
// share mode of 0 grants exclusive access, and the OS releases it when the
// handle closes -- including on crash, which is the property we rely on.
func openExclusive(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	return syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
}

func tryExclusive(path string) (io.Closer, bool, error) {
	h, err := openExclusive(path)
	if err != nil {
		if err == errSharingViolation {
			return nil, false, nil // held by another process
		}
		return nil, false, fmt.Errorf("open lock: %w", err)
	}
	return &winLock{h: h}, true, nil
}

// blockingExclusive retries, since Windows has no blocking equivalent of
// flock(LOCK_EX) on a share-mode open.
func blockingExclusive(path string) (io.Closer, error) {
	for i := 0; i < 600; i++ {
		c, ok, err := tryExclusive(path)
		if err != nil {
			return nil, err
		}
		if ok {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for lock %s", path)
}

func lockIsFree(path string) bool {
	c, ok, err := tryExclusive(path)
	if err != nil || !ok {
		return false
	}
	_ = c.Close()
	return true
}

func processExists(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	const stillActive = 259
	return code == stillActive
}
