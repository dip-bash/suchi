package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSingleInstanceLock(t *testing.T) {
	pid := filepath.Join(t.TempDir(), "state", "suchi.pid")

	if running, _ := Status(pid); running {
		t.Fatal("nothing holds the lock yet")
	}
	l, err := Acquire(pid)
	if err != nil {
		t.Fatal(err)
	}
	running, got := Status(pid)
	if !running || got != os.Getpid() {
		t.Fatalf("status = %v pid=%d, want running with our pid", running, got)
	}

	var busy ErrRunning
	if _, err := Acquire(pid); !errors.As(err, &busy) || busy.PID != os.Getpid() {
		t.Fatalf("second Acquire must fail with ErrRunning{%d}, got %v", os.Getpid(), err)
	}
	if msg := busy.Error(); msg == "" || !contains(msg, strconv.Itoa(os.Getpid())) {
		t.Fatalf("message should name the pid: %q", msg)
	}

	l.Release()
	if running, _ := Status(pid); running {
		t.Fatal("lock must be free after Release")
	}
	l2, err := Acquire(pid)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	l2.Release()
}

func TestStopWhenNotRunning(t *testing.T) {
	stopped, err := Stop(filepath.Join(t.TempDir(), "suchi.pid"))
	if err != nil || stopped {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
