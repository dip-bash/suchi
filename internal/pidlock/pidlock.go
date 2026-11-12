// Package pidlock is a single-instance guard built on flock(2): the process
// that holds the lock writes its pid into the file. The kernel drops the lock
// when the process dies, so a crash never leaves a stale lock behind, and a
// held lock proves its holder is alive (no pid-reuse guessing).
package pidlock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Lock is a held lock.
type Lock struct{ f *os.File }

// ErrHeld means another process holds the lock; PID is that process (0 if it
// has not written its pid yet).
type ErrHeld struct{ PID int }

func (e ErrHeld) Error() string { return fmt.Sprintf("already held by pid %d", e.PID) }

// Acquire takes the exclusive lock without waiting and records our pid.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPID(f)
		_ = f.Close()
		return nil, ErrHeld{PID: pid}
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = l.f.Truncate(0)
		_ = l.f.Close() // closing releases the flock
		l.f = nil
	}
}

func readPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	return pid
}

// Holder reports whether the lock is held and by which pid.
func Holder(path string) (held bool, pid int) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return false, 0
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return false, 0 // we got the lock, so nobody holds it
	}
	return true, readPID(f)
}

// Takeover makes this process the holder: if another process holds the lock
// it is asked to exit with SIGTERM (re-sent every second) and the lock is
// taken as soon as it lets go. It never uses SIGKILL, because a process
// that is killed cannot put its terminal back in order; if the holder does
// not exit within wait, Takeover gives up with an error.
func Takeover(ctx context.Context, path string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	var lastSignal time.Time
	for {
		l, err := Acquire(path)
		if err == nil {
			return l, nil
		}
		held, ok := err.(ErrHeld)
		if !ok {
			return nil, err
		}
		if held.PID > 0 && held.PID != os.Getpid() && time.Since(lastSignal) >= time.Second {
			_ = syscall.Kill(held.PID, syscall.SIGTERM)
			lastSignal = time.Now()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the previous suchi (pid %d) did not close within %s - it may be busy "+
				"running a command and will close when that finishes; then run suchi again", held.PID, wait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
}
