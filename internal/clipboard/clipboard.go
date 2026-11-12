// Package clipboard talks to the system clipboard on Wayland, X11 and plain
// terminals.
//
//   - wayland: native protocol client (ext-data-control / wlr-data-control),
//     no wl-clipboard needed. Works on sway, Hyprland, KDE Plasma, river,
//     labwc, COSMIC... but not on GNOME/Mutter, which offers no background
//     clipboard protocol (suchi then falls back to XWayland via x11).
//   - x11: native XFixes watcher; xclip or xsel read and write the selection.
//   - tty: no display server (Linux console, SSH). There is nothing to watch;
//     choosing an entry uses OSC 52 or types it into the prompt.
package clipboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Selection is which X/Wayland selection to use.
type Selection string

// Supported selections.
const (
	Clipboard Selection = "clipboard"
	Primary   Selection = "primary"
)

// HelperArg is the hidden command-line argument the binary is re-executed
// with to keep serving a Wayland selection after the TUI exits.
const HelperArg = "__serve"

// Sentinel errors.
var (
	// ErrNoWatch means the backend cannot observe clipboard changes (tty).
	ErrNoWatch = errors.New("this backend cannot watch the clipboard")
	// ErrNoClipboard means nothing could take the text (tty without OSC 52).
	ErrNoClipboard = errors.New("no clipboard available")
)

// Backend is one way of reaching a clipboard.
type Backend interface {
	// Name is "wayland", "x11" or "tty".
	Name() string
	// Watch calls fn with every clipboard change (and once with the current
	// content at start) until ctx is cancelled: text, images and copied
	// files. Entries over the limits and entries flagged secret are skipped.
	Watch(ctx context.Context, sel Selection, lim Limits, fn func(Item)) error
	// Set puts the item on the selection and returns once it is
	// established; it stays available after the calling process exits.
	Set(ctx context.Context, it Item, sel Selection) error
}

// Detect picks a backend. pref is "auto", "wayland", "x11" or "tty". For
// "auto" it returns the tty backend plus a human-readable problem when a
// graphical session exists but cannot be used (missing xclip, GNOME, ...).
func Detect(pref string) (b Backend, problem string, err error) {
	switch pref {
	case "tty":
		return TTY{}, "", nil
	case "wayland":
		if err := probeWayland(); err != nil {
			return nil, "", err
		}
		return Wayland{}, "", nil
	case "x11":
		x, err := newX11()
		if err != nil {
			return nil, "", err
		}
		return x, "", nil
	}

	var problems []string
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		err := probeWayland()
		if err == nil {
			return Wayland{}, "", nil
		}
		problems = append(problems, err.Error())
	}
	if os.Getenv("DISPLAY") != "" {
		x, err := newX11()
		if err == nil {
			return x, "", nil
		}
		problems = append(problems, err.Error())
	}
	return TTY{}, strings.Join(problems, " | "), nil
}

// Deliver puts an item on the best available clipboard. If the chosen
// backend fails it falls back to the terminal (OSC 52 / typing it into the
// prompt) and finally prints the text, so a choice is never silently lost.
// Diagnostics go to errw; the last-resort print goes to out.
func Deliver(ctx context.Context, b Backend, problem string, it Item, sel Selection, out, errw io.Writer) error {
	if b.Name() != "tty" {
		err := b.Set(ctx, it, sel)
		if err == nil {
			return nil
		}
		fmt.Fprintf(errw, "suchi: %v\n", err)
	} else if problem != "" {
		fmt.Fprintf(errw, "suchi: %s\n", problem)
	}
	if it.Kind == KindImage {
		return errors.New("an image cannot be pasted into a terminal; use a graphical session")
	}
	if err := (TTY{}).Set(ctx, it, sel); err == nil {
		return nil
	}
	text := it.Plain()
	if isTerminal(out) {
		fmt.Fprintln(errw, "suchi: no clipboard available here; entry printed below "+
			"(use `suchi -p` with a shell binding to paste it, see README)")
		_, err := fmt.Fprintln(out, SanitizeForTerminal(text))
		return err
	}
	_, err := io.WriteString(out, text)
	return err
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// SanitizeForTerminal replaces control characters so clipboard text can never
// inject escape sequences when printed to a terminal.
func SanitizeForTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return '·'
		}
		return r
	}, s)
}
