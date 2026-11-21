package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/dip-bash/suchi/internal/preview"
	"github.com/dip-bash/suchi/internal/store"
)

// Options configure Run.
type Options struct {
	ASCII    bool             // force plain-ASCII glyphs (automatic on the Linux console)
	Snips    bool             // open the Snippets tab first
	Protocol preview.Protocol // image protocol; empty = detect from the environment
	Out      io.Writer        // where graphics sequences are written; nil = the terminal
}

// Run shows the interface and returns what the user chose. It works even
// when stdin or stdout are pipes (`$(suchi -p)`), by talking to /dev/tty.
func Run(ctx context.Context, st *store.Store, opts Options) (Result, error) {
	restore, err := attachTTY()
	if err != nil {
		return Result{}, err
	}
	defer restore()

	if opts.Protocol == "" {
		opts.Protocol = preview.Detect(os.Getenv)
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	m := newModel(ctx, st, opts)
	m.cellW, m.cellH = preview.CellSize()
	// Bubble Tea's own signal handler and context support are NOT used: both
	// can deadlock when a signal arrives while the program is starting or
	// stopping (its handler does an unconditional send into a channel nobody
	// reads any more, and shutdown waits for it forever). Signals are bridged
	// through Program.Quit instead, which is safe at any moment.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler())
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	stop := bridgeSignals(ctx, p.Quit, sigc, &m.execing)
	_, err = p.Run()
	stop()
	signal.Stop(sigc)
	zone.Close()
	if opts.Protocol == preview.Kitty { // never leave a picture behind in the terminal
		_, _ = io.WriteString(opts.Out, preview.KittyDelete())
	}
	if err != nil {
		if ctx.Err() != nil { // told to close (a newer suchi opened): leave quietly
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("terminal UI: %w", err)
	}
	return m.result, nil
}

// bridgeSignals asks the program to quit when a termination signal arrives
// (or ctx is cancelled). SIGINT is ignored while a command snippet runs in the
// foreground: Ctrl+C then belongs to that command, not to suchi. SIGTERM and
// SIGHUP are never ignored; if a command is running the quit simply waits
// until it finishes. The returned function stops the bridge.
func bridgeSignals(ctx context.Context, quit func(), sigc <-chan os.Signal, execing *atomic.Bool) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case s := <-sigc:
				if s == syscall.SIGINT && execing.Load() {
					continue
				}
				quit()
				return
			case <-ctx.Done():
				quit()
				return
			case <-done:
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func isChar(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// attachTTY points fds 0 and 1 at the controlling terminal when they are
// redirected, and returns a function that puts them back.
func attachTTY() (restore func(), err error) {
	var saved [2]int
	needs := [2]bool{!isChar(os.Stdin), !isChar(os.Stdout)}
	if !needs[0] && !needs[1] {
		return func() {}, nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("suchi's interface needs a terminal (no /dev/tty): %w", err)
	}
	for fd := 0; fd < 2; fd++ {
		if !needs[fd] {
			continue
		}
		if saved[fd], err = syscall.Dup(fd); err != nil {
			_ = tty.Close()
			return nil, err
		}
		if err := syscall.Dup3(int(tty.Fd()), fd, 0); err != nil {
			_ = tty.Close()
			return nil, err
		}
	}
	return func() {
		for fd := 0; fd < 2; fd++ {
			if needs[fd] {
				_ = syscall.Dup3(saved[fd], fd, 0)
				_ = syscall.Close(saved[fd])
			}
		}
		_ = tty.Close()
	}, nil
}
