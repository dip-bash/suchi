// Package daemon runs the background clipboard watcher: it records every
// clipboard change into the store. Only one daemon runs per state directory,
// guarded by an flock on the pid file that the kernel drops if the process dies.
package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dip-bash/suchi/internal/clipboard"
	"github.com/dip-bash/suchi/internal/config"
	"github.com/dip-bash/suchi/internal/pidlock"
	"github.com/dip-bash/suchi/internal/store"
)

// ErrRunning is returned when another daemon already holds the lock.
type ErrRunning struct{ PID int }

func (e ErrRunning) Error() string {
	return fmt.Sprintf("daemon is already running (pid %d); stop it with `suchi stop`", e.PID)
}

// Lock is the single-instance guard.
type Lock = pidlock.Lock

// Acquire takes the daemon lock, or reports that a daemon is already running.
func Acquire(path string) (*Lock, error) {
	l, err := pidlock.Acquire(path)
	var held pidlock.ErrHeld
	if errors.As(err, &held) {
		return nil, ErrRunning{PID: held.PID}
	}
	return l, err
}

// Status reports whether a daemon is running and its pid.
func Status(pidPath string) (running bool, pid int) { return pidlock.Holder(pidPath) }

// Stop asks a running daemon to exit and waits for it.
func Stop(pidPath string) (stopped bool, err error) {
	running, pid := Status(pidPath)
	if !running {
		return false, nil
	}
	if pid <= 0 {
		return false, errors.New("daemon is running but its pid file is unreadable")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("signal pid %d: %w", pid, err)
	}
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if running, _ := Status(pidPath); !running {
			return true, nil
		}
	}
	return false, fmt.Errorf("sent SIGTERM to %d but it is still running", pid)
}

// Foreground runs the watcher in this process until ctx is cancelled. ready
// is called exactly once: with nil when the daemon is up and watching, or
// with the error that prevented it from starting.
func Foreground(ctx context.Context, cfg config.Config, ready func(error)) error {
	started := false
	signal := func(err error) {
		if !started {
			started = true
			if ready != nil {
				ready(err)
			}
		}
	}

	b, err := Backend(cfg)
	if err != nil {
		signal(err)
		return err
	}
	lock, err := Acquire(cfg.PIDPath())
	if err != nil {
		signal(err)
		return err
	}
	defer lock.Release()
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		signal(err)
		return err
	}
	defer st.Close()

	sel := clipboard.Selection(cfg.Selection)
	slog.Info("watching clipboard", "backend", b.Name(), "selection", cfg.Selection, "db", cfg.DBPath)
	signal(nil)
	lim := store.Limits{Count: cfg.Limit, MaxText: cfg.MaxBytes, MaxMedia: cfg.MaxMedia, MediaBudget: config.DefaultMediaBudget}
	err = b.Watch(ctx, sel, clipboard.Limits{MaxText: cfg.MaxBytes, MaxMedia: cfg.MaxMedia}, func(it clipboard.Item) {
		if _, err := st.AddItem(ctx, newClip(it), lim); err != nil && ctx.Err() == nil {
			slog.Error("could not save clipboard entry", "err", err)
		}
	})
	if err != nil {
		slog.Error("watcher stopped", "err", err)
	}
	return err
}

// Start launches a detached daemon (a re-executed copy of this binary with
// `daemon --foreground`) and waits until it reports it is up.
func Start(cfg config.Config) (pid int, err error) {
	if running, p := Status(cfg.PIDPath()); running {
		return p, ErrRunning{PID: p}
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return 0, err
	}
	trimLog(cfg.LogPath())
	logf, err := os.OpenFile(cfg.LogPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer pr.Close()

	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "daemon", "--foreground", "--ready-fd", "3",
		"--db", cfg.DBPath,
		"--limit", strconv.Itoa(cfg.Limit),
		"--max-size", strconv.Itoa(cfg.MaxBytes),
		"--max-media", strconv.Itoa(cfg.MaxMedia),
		"--selection", cfg.Selection,
		"--backend", cfg.Backend)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return 0, fmt.Errorf("start daemon: %w", err)
	}
	_ = pw.Close()

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(pr).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case s := <-line:
		if s == "ok" {
			pid = cmd.Process.Pid
			_ = cmd.Process.Release()
			return pid, nil
		}
		_ = cmd.Wait()
		if msg, ok := strings.CutPrefix(s, "err:"); ok {
			return 0, errors.New(strings.TrimSpace(strings.ReplaceAll(msg, "\\n", "\n")))
		}
		return 0, fmt.Errorf("daemon exited during start-up; see %s", cfg.LogPath())
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("daemon did not start in time; see %s", cfg.LogPath())
	}
}

// trimLog keeps the log from growing without bound.
func trimLog(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() > 256*1024 {
		_ = os.Remove(path)
	}
}

// Backend picks the clipboard backend for the daemon. A terminal has no
// clipboard to watch, so the tty backend is reported as an error.
func Backend(cfg config.Config) (clipboard.Backend, error) {
	b, problem, err := clipboard.Detect(cfg.Backend)
	if err != nil {
		return nil, err
	}
	if b.Name() == "tty" {
		msg := "no display server to watch (Linux console / SSH): a terminal has no clipboard. " +
			"Feed the history with `echo text | suchi add` instead"
		if problem != "" {
			msg = problem + "\n" + msg
		}
		return nil, errors.New(msg)
	}
	return b, nil
}

// newClip converts what the clipboard backend saw into a store entry.
func newClip(it clipboard.Item) store.NewClip {
	switch it.Kind {
	case clipboard.KindImage:
		return store.NewClip{Kind: store.ClipImage, Mime: it.MIME, Data: it.Data}
	case clipboard.KindFiles:
		return store.NewClip{Kind: store.ClipFile, Mime: it.MIME, Text: it.Text}
	}
	return store.NewClip{Kind: store.ClipText, Text: it.Text}
}
