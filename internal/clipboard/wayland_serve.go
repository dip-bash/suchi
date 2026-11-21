package clipboard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Set implements Backend. A Wayland selection only exists while its owner is
// alive, so the text is handed to a detached copy of this binary (see
// ServeSelection) that keeps answering paste requests, like wl-copy does.
// Go cannot fork safely, hence the re-exec.
func (Wayland) Set(_ context.Context, it Item, sel Selection) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}
	cmd := exec.Command(exe, HelperArg, string(sel)) // no ctx: the helper must outlive the caller
	var in bytes.Buffer
	if err := encodeItem(&in, it); err != nil {
		return err
	}
	cmd.Stdin = &in
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start clipboard helper: %w", err)
	}
	reply := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		reply <- strings.TrimSpace(line)
	}()
	select {
	case line := <-reply:
		if line == "ok" {
			_ = cmd.Process.Release() // keep serving after we exit
			return nil
		}
		_ = cmd.Wait()
		if msg, ok := strings.CutPrefix(line, "err:"); ok {
			return errors.New(strings.TrimSpace(msg))
		}
		return errors.New("clipboard helper failed")
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("clipboard helper timed out")
	}
}

// ServeSelection is the body of the hidden helper process: read the item
// from in, take ownership of the selection, report "ok" (or "err: ...") on
// ready, then serve pastes until the selection is replaced.
func ServeSelection(ctx context.Context, sel Selection, in io.Reader, ready io.Writer) error {
	it, err := decodeItem(in)
	if err != nil {
		fmt.Fprintf(ready, "err: %v\n", err)
		return err
	}
	w, err := dialWayland()
	if err != nil {
		fmt.Fprintf(ready, "err: %v\n", err)
		return err
	}
	defer w.close()
	if err := w.setSelection(it, sel); err != nil {
		fmt.Fprintf(ready, "err: %v\n", err)
		return err
	}
	fmt.Fprintln(ready, "ok")
	return w.serve(ctx)
}
