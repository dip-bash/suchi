package clipboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// X11 is the X11 backend. Changes are detected with a native XFixes
// connection (see xproto.go); the content itself is read and written with
// xclip (text, images, copied files) or xsel (text only). It also serves
// XWayland sessions such as GNOME on Wayland.
type X11 struct{ tool string }

func newX11() (X11, error) {
	if os.Getenv("DISPLAY") == "" {
		return X11{}, errors.New("DISPLAY is not set; no X11 server to talk to")
	}
	for _, t := range []string{"xclip", "xsel"} {
		if _, err := exec.LookPath(t); err == nil {
			return X11{tool: t}, nil
		}
	}
	return X11{}, errors.New("xclip (or xsel) not found; install it, e.g. `sudo apt install xclip`")
}

// Name implements Backend.
func (X11) Name() string { return "x11" }

func (x X11) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return exec.CommandContext(ctx, args[0], args[1:]...).Output()
}

// targets lists the formats the selection owner offers (xclip only).
func (x X11) targets(ctx context.Context, sel Selection) []string {
	if x.tool != "xclip" {
		return nil
	}
	out, err := x.run(ctx, 2*time.Second, "xclip", "-selection", string(sel), "-t", "TARGETS", "-o")
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

func (x X11) readTarget(ctx context.Context, sel Selection, target string, maxBytes int) ([]byte, bool) {
	var out []byte
	var err error
	switch {
	case x.tool == "xclip" && target != "":
		out, err = x.run(ctx, 3*time.Second, "xclip", "-selection", string(sel), "-t", target, "-o")
	case x.tool == "xclip":
		out, err = x.run(ctx, 2*time.Second, "xclip", "-selection", string(sel), "-o")
	default:
		out, err = x.run(ctx, 2*time.Second, "xsel", "--"+string(sel), "--output")
	}
	if err != nil || len(out) > maxBytes { // empty selection, wrong target, owner gone, or too big
		return nil, false
	}
	return out, true
}

// read returns what is currently on the selection.
func (x X11) read(ctx context.Context, sel Selection, lim Limits) (Item, bool) {
	targets := x.targets(ctx, sel)
	if len(targets) == 0 { // xsel, or an owner that does not list targets: plain text
		data, ok := x.readTarget(ctx, sel, "", max(lim.MaxText, 1))
		if !ok {
			return Item{}, false
		}
		return buildItem(candidate{KindText, ""}, data, lim)
	}
	for _, c := range candidates(targets) {
		data, ok := x.readTarget(ctx, sel, c.mime, maxFor(c, lim))
		if !ok {
			return Item{}, false
		}
		if it, ok := buildItem(c, data, lim); ok {
			return it, true
		} else if c.kind != KindFiles {
			return Item{}, false
		}
	}
	return Item{}, false
}

// Set implements Backend. xclip/xsel fork into the background to keep
// serving the selection, so the content outlives this process.
func (x X11) Set(ctx context.Context, it Item, sel Selection) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	var body []byte
	switch {
	case it.Kind == KindText && x.tool == "xclip":
		cmd, body = exec.CommandContext(ctx, "xclip", "-selection", string(sel), "-i"), []byte(it.Text)
	case it.Kind == KindText:
		cmd, body = exec.CommandContext(ctx, "xsel", "--"+string(sel), "--input"), []byte(it.Text)
	case x.tool != "xclip":
		return errors.New("copying images and files on X11 needs xclip (xsel only handles text)")
	default:
		// GNOME's file manager reads x-special/gnome-copied-files; everything else uses text/uri-list.
		want := mimeURIList
		if strings.Contains(os.Getenv("XDG_CURRENT_DESKTOP"), "GNOME") {
			want = mimeGnomeCopy
		}
		target := it.MIME
		body = it.Data
		if it.Kind == KindFiles {
			target = want
			for _, f := range Formats(it) {
				if f.MIME == want {
					body = f.Data
				}
			}
		}
		cmd = exec.CommandContext(ctx, "xclip", "-selection", string(sel), "-t", target, "-i")
	}
	cmd.Stdin = bytes.NewReader(body)
	// Own session: the process that keeps serving the selection must survive
	// this terminal closing (SIGHUP) just like it survives suchi exiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil // still serving the selection; that is fine
		}
		return fmt.Errorf("%s failed: %w (is DISPLAY correct?)", x.tool, err)
	}
	return nil
}

func itemKey(it Item) string {
	h := sha256.Sum256(it.Data)
	return string(it.Kind) + "\x00" + it.MIME + "\x00" + it.Text + "\x00" + string(h[:])
}

// Watch implements Backend.
func (x X11) Watch(ctx context.Context, sel Selection, lim Limits, fn func(Item)) error {
	last := ""
	have := false
	emit := func() {
		it, ok := x.read(ctx, sel, lim)
		if !ok {
			return
		}
		if k := itemKey(it); !have || k != last {
			last, have = k, true
			fn(it)
		}
	}

	events, err := watchXFixes(ctx, sel)
	if err != nil {
		slog.Warn("XFixes unavailable, polling the clipboard instead", "reason", err)
		emit()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				emit()
			}
		}
	}
	emit() // record whatever is on the clipboard at startup
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("lost the connection to the X server")
			}
			if err != nil {
				return err
			}
			time.Sleep(50 * time.Millisecond) // let the new owner finish setting up
			for drained := false; !drained; { // coalesce a burst into one read
				select {
				case _, open := <-events:
					if !open {
						drained = true // the next receive reports the closed channel
					}
				default:
					drained = true
				}
			}
			emit()
		}
	}
}
