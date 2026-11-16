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
	if err := f.Truncate(0); err != nil {
		_ = f.Close() // closing releases the flock
		return nil, fmt.Errorf("clear pid lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		_ = f.Close() // closing releases the flock
		return nil, fmt.Errorf("write pid lock file: %w", err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		// Leave the last PID as harmless diagnostic/stale data. Clearing it
		// before Close creates a window where the lock is held but no PID can
		// be signalled by a concurrent Takeover.
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

// Takeover makes this process the holder. If another process owns the lock, it
// first receives SIGTERM so its TUI can restore the terminal and exit normally.
// If the same process still owns the lock after wait, it receives SIGKILL as a
// last resort. The PID is re-read from the lock file immediately before SIGKILL
// and the lock is re-checked, so a stale PID file is never enough to kill a
// process. A killed holder cannot restore its terminal; this is intentionally
// only the fallback after graceful shutdown has failed.
func Takeover(ctx context.Context, path string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	var signalledPID int
	killed := false
	killDeadline := time.Time{}
	const poll = 30 * time.Millisecond
	const killGrace = time.Second

	for {
		l, err := Acquire(path)
		if err == nil {
			return l, nil
		}
		held, ok := err.(ErrHeld)
		if !ok {
			return nil, err
		}

		if held.PID > 0 && held.PID != os.Getpid() && held.PID != signalledPID {
			// Signal once per holder. A later launcher may have taken the lock
			// while we were waiting, in which case it gets its own TERM.
			_ = syscall.Kill(held.PID, syscall.SIGTERM)
			signalledPID = held.PID
		}

		now := time.Now()
		if !killed && !now.Before(deadline) {
			// Confirm that someone still owns the lock, and use the PID currently
			// written by that holder rather than the potentially stale PID above.
			stillHeld, currentPID := Holder(path)
			if !stillHeld {
				continue // the next Acquire should win
			}
			if currentPID <= 0 || currentPID == os.Getpid() {
				return nil, fmt.Errorf("the previous suchi window still holds the UI lock (pid %d); "+
					"could not identify its process safely", currentPID)
			}
			if err := syscall.Kill(currentPID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return nil, fmt.Errorf("could not stop previous suchi window (pid %d): %w", currentPID, err)
			}
			killed = true
			killDeadline = now.Add(killGrace)
		}
		if killed && !time.Now().Before(killDeadline) {
			return nil, fmt.Errorf("previous suchi window (pid %d) did not release the UI lock after SIGKILL", held.PID)
		}

		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
