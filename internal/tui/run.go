package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/dip-bash/suchi/internal/preview"
	"github.com/dip-bash/suchi/internal/store"
)

// Options configure Run.
type Options struct {
	ASCII    bool             // force plain-ASCII glyphs (automatic on the Linux console)
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
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	_, err = p.Run()
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
