package pidlock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the "other suchi" process: the tests re-run this test
// binary with PIDLOCK_HELPER set to get a real second process holding the lock.
func TestMain(m *testing.M) {
	if mode := os.Getenv("PIDLOCK_HELPER"); mode != "" {
		helper(mode, os.Getenv("PIDLOCK_PATH"))
		return
	}
	os.Exit(m.Run())
}

func helper(mode, path string) {
	// Like suchi itself: set up signal handling BEFORE taking the lock.
	c := make(chan os.Signal, 1)
	if mode == "stubborn" { // ignores SIGTERM, like a process stuck in a long command
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(c, syscall.SIGTERM)
	}
	l, err := Acquire(path)
	if err != nil {
		fmt.Println("busy")
		os.Exit(2)
	}
	fmt.Println("locked")
	if mode == "stubborn" {
		time.Sleep(20 * time.Second)
		return
	}
	<-c
	l.Release()
	os.Exit(0)
}

func startHelper(t *testing.T, mode, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PIDLOCK_HELPER="+mode, "PIDLOCK_PATH="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper did not get the lock: %q", line)
	}
	return cmd
}

func lockPath(t *testing.T) string { return filepath.Join(t.TempDir(), "state", "suchi-ui.pid") }

func TestAcquireHolderRelease(t *testing.T) {
	p := lockPath(t)
	if held, _ := Holder(p); held {
		t.Fatal("nothing holds the lock yet")
	}
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if held, pid := Holder(p); !held || pid != os.Getpid() {
		t.Fatalf("Holder = %v, %d", held, pid)
	}
	var h ErrHeld
	if _, err := Acquire(p); !errors.As(err, &h) || h.PID != os.Getpid() {
		t.Fatalf("second Acquire must report us as holder, got %v", err)
	}
	l.Release()
	l.Release() // releasing twice is harmless
	if held, _ := Holder(p); held {
		t.Fatal("released")
	}
}

func TestStalePidFileDoesNotBlock(t *testing.T) {
	p := lockPath(t)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte("999999"), 0o600) // left over by a crashed process: nobody holds the flock
	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("a stale pid file must not block: %v", err)
	}
	l.Release()
}

func TestTakeoverClosesThePreviousHolder(t *testing.T) {
	p := lockPath(t)
	old := startHelper(t, "polite", p)
	pidBefore := old.Process.Pid
	if held, pid := Holder(p); !held || pid != pidBefore {
		t.Fatalf("helper should hold the lock: %v %d", held, pid)
	}

	start := time.Now()
	l, err := Takeover(context.Background(), p, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if time.Since(start) > 2*time.Second {
		t.Fatalf("takeover took %v; it must be quick", time.Since(start))
	}
	if err := old.Wait(); err != nil {
		t.Fatalf("the previous instance must exit cleanly (SIGTERM handled): %v", err)
	}
	if held, pid := Holder(p); !held || pid != os.Getpid() {
		t.Fatalf("we must be the holder now: %v %d", held, pid)
	}
	data, _ := os.ReadFile(p)
	if got, _ := strconv.Atoi(strings.TrimSpace(string(data))); got != os.Getpid() {
		t.Fatalf("pid file says %q", data)
	}
}

func TestTakeoverWithNoHolderIsImmediate(t *testing.T) {
	start := time.Now()
	l, err := Takeover(context.Background(), lockPath(t), time.Second)
	if err != nil || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	l.Release()
}

// A holder that will not close is reported, never SIGKILLed (that would leave its terminal broken).
func TestTakeoverGivesUpOnAStubbornHolder(t *testing.T) {
	p := lockPath(t)
	stubborn := startHelper(t, "stubborn", p)
	_, err := Takeover(context.Background(), p, 400*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(stubborn.Process.Pid)) {
		t.Fatalf("want an error naming the holder, got %v", err)
	}
	if err := stubborn.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("the stubborn holder must still be alive (no SIGKILL)")
	}
}

func TestTakeoverStopsWhenContextIsCancelled(t *testing.T) {
	p := lockPath(t)
	startHelper(t, "stubborn", p)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := Takeover(ctx, p, 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
