package clipboard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode"
	"unsafe"
)

// TTY is the fallback for sessions without a display server (Linux console,
// SSH). There is no clipboard to watch, so Watch is unsupported; Set uses the
// terminal itself: OSC 52 where the terminal supports it, otherwise typing a
// single clean line into the prompt (TIOCSTI).
type TTY struct{}

// Name implements Backend.
func (TTY) Name() string { return "tty" }

// Watch implements Backend; a terminal has no clipboard to observe.
func (TTY) Watch(context.Context, Selection, Limits, func(Item)) error { return ErrNoWatch }

// maxOSC52 keeps the escape sequence below what most terminals accept.
const maxOSC52 = 70000

// Set implements Backend.
func (TTY) Set(_ context.Context, it Item, _ Selection) error {
	text := it.Plain()
	if it.Kind == KindImage || text == "" {
		return ErrNoClipboard
	}
	term := os.Getenv("TERM")
	if term != "" && term != "linux" && term != "dumb" && len(text) <= maxOSC52 {
		if err := osc52(text); err == nil {
			fmt.Fprintln(os.Stderr, "suchi: sent to the terminal clipboard (OSC 52; needs terminal support)")
			return nil
		}
	}
	if term == "linux" && injectable(text) {
		if err := inject(text); err == nil {
			return nil
		}
	}
	return ErrNoClipboard
}

func osc52(text string) error {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer tty.Close()
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	if os.Getenv("TMUX") != "" {
		cmd := exec.Command("tmux", "load-buffer", "-")
		cmd.Stdin = strings.NewReader(text)
		_ = cmd.Run() // best effort: fill tmux's own buffer too
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	_, err = tty.WriteString(seq)
	return err
}

// injectable allows only a single clean line: typing newlines or control
// characters into a shell would execute them.
func injectable(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

// inject types text into the controlling terminal's input queue. Kernels
// >= 6.2 may disable TIOCSTI (dev.tty.legacy_tiocsti); then this fails.
func inject(text string) error {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer tty.Close()
	for _, b := range []byte(text) {
		c := b
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), uintptr(syscall.TIOCSTI), uintptr(unsafe.Pointer(&c))); e != 0 {
			return errors.New(e.Error())
		}
	}
	return nil
}
