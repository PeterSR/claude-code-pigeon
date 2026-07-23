//go:build !windows

package pigeon

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// MonitorSupported reports whether a monitor can run on this platform.
//
// Claude Code only arms plugin monitors in interactive CLI sessions on Unix,
// so this mirrors that rather than pretending otherwise.
const MonitorSupported = true

type unixLock struct{ f *os.File }

func (l *unixLock) Close() error {
	// Releasing before closing is redundant -- the kernel drops the lock when
	// the descriptor closes -- but it makes the intent explicit.
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}

// tryExclusive takes an exclusive lock held for as long as the returned Closer
// is open. It reports false (without error) when another process holds it.
//
// The lock file is deliberately never unlinked: removing it would let a second
// process lock a different inode while both believed they held it.
func tryExclusive(path string) (io.Closer, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false, nil
	}
	return &unixLock{f: f}, true, nil
}

// blockingExclusive waits for the lock rather than giving up.
func blockingExclusive(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock: %w", err)
	}
	return &unixLock{f: f}, nil
}

// lockIsFree reports whether nobody currently holds the lock. This is how a
// dead monitor is detected: the kernel drops the lock the instant the holding
// process exits, however it exits.
func lockIsFree(path string) bool {
	c, ok, err := tryExclusive(path)
	if err != nil {
		return false // cannot tell; do not raise a false alarm
	}
	if !ok {
		return false
	}
	_ = c.Close()
	return true
}

// processExists reports whether a pid is live. EPERM means it exists but
// belongs to someone else, which still counts.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// isExecutable reports whether a file can be run. Any execute bit will do: the
// plugin binary only has to be runnable by whoever starts the session.
func isExecutable(fi os.FileInfo) bool { return fi.Mode()&0o111 != 0 }
