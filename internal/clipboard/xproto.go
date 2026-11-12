package clipboard

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A tiny X11 wire-protocol client: just enough to connect, intern the
// CLIPBOARD atom and subscribe to XFixes SelectionNotify events, so suchi
// needs neither libX11 nor cgo and wakes up instantly on a copy.

var le = binary.LittleEndian

type xconn struct {
	c         net.Conn
	r         *bufio.Reader
	root      uint32
	extEvent  byte
	extOpcode byte
}

func pad4(n int) int { return (4 - n%4) % 4 }

// displayNumber extracts N from DISPLAY values like ":0", ":0.0" or "unix:0".
func displayNumber(d string) (string, error) {
	rest, ok := strings.CutPrefix(d, "unix:")
	if !ok {
		rest, ok = strings.CutPrefix(d, ":")
	}
	if !ok {
		return "", fmt.Errorf("DISPLAY %q is not a local display", d)
	}
	return strings.SplitN(rest, ".", 2)[0], nil
}

// cookie finds the MIT-MAGIC-COOKIE-1 for this display in ~/.Xauthority.
func cookie(num string) []byte {
	path := os.Getenv("XAUTHORITY")
	if path == "" {
		if h, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(h, ".Xauthority")
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	host, _ := os.Hostname()
	read := func() ([]byte, bool) {
		if len(b) < 2 {
			return nil, false
		}
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n {
			return nil, false
		}
		v := b[2 : 2+n]
		b = b[2+n:]
		return v, true
	}
	for len(b) >= 2 {
		family := binary.BigEndian.Uint16(b)
		b = b[2:]
		addr, ok1 := read()
		number, ok2 := read()
		name, ok3 := read()
		data, ok4 := read()
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return nil
		}
		if string(name) != "MIT-MAGIC-COOKIE-1" {
			continue
		}
		if (number == nil || string(number) == "" || string(number) == num) &&
			(family == 65535 || (family == 256 && string(addr) == host)) {
			return data
		}
	}
	return nil
}

func dialX11() (*xconn, error) {
	num, err := displayNumber(os.Getenv("DISPLAY"))
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("unix", "/tmp/.X11-unix/X"+num, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to X server: %w", err)
	}
	x := &xconn{c: c, r: bufio.NewReader(c)}
	if err := x.handshake(cookie(num)); err != nil {
		_ = c.Close()
		return nil, err
	}
	return x, nil
}

func (x *xconn) handshake(auth []byte) error {
	name := []byte("MIT-MAGIC-COOKIE-1")
	if len(auth) == 0 {
		name = nil
	}
	req := make([]byte, 12)
	req[0] = 'l'
	le.PutUint16(req[2:], 11)
	le.PutUint16(req[6:], uint16(len(name)))
	le.PutUint16(req[8:], uint16(len(auth)))
	req = append(req, name...)
	req = append(req, make([]byte, pad4(len(name)))...)
	req = append(req, auth...)
	req = append(req, make([]byte, pad4(len(auth)))...)
	_ = x.c.SetDeadline(time.Now().Add(3 * time.Second))
	defer x.c.SetDeadline(time.Time{})
	if _, err := x.c.Write(req); err != nil {
		return err
	}
	var head [8]byte
	if _, err := io.ReadFull(x.r, head[:]); err != nil {
		return err
	}
	body := make([]byte, 4*int(le.Uint16(head[6:])))
	if _, err := io.ReadFull(x.r, body); err != nil {
		return err
	}
	if head[0] != 1 {
		reason := ""
		if n := int(head[1]); n <= len(body) {
			reason = string(body[:n])
		}
		return fmt.Errorf("X server refused the connection: %s", reason)
	}
	vendor := int(le.Uint16(body[16:]))
	formats := int(body[21])
	off := 32 + vendor + pad4(vendor) + 8*formats
	if off+4 > len(body) {
		return errors.New("malformed X server setup reply")
	}
	x.root = le.Uint32(body[off:])
	return nil
}

func (x *xconn) request(opcode, data byte, body []byte) error {
	body = append(body, make([]byte, pad4(len(body)))...)
	msg := make([]byte, 4, 4+len(body))
	msg[0], msg[1] = opcode, data
	le.PutUint16(msg[2:], uint16(1+len(body)/4))
	_, err := x.c.Write(append(msg, body...))
	return err
}

// reply reads until the reply to the last request, skipping any events.
func (x *xconn) reply() ([]byte, error) {
	for {
		var b [32]byte
		if _, err := io.ReadFull(x.r, b[:]); err != nil {
			return nil, err
		}
		switch b[0] {
		case 0:
			return nil, fmt.Errorf("X protocol error %d", b[1])
		case 1:
			extra := make([]byte, 4*int(le.Uint32(b[4:])))
			if _, err := io.ReadFull(x.r, extra); err != nil {
				return nil, err
			}
			return append(b[:], extra...), nil
		}
	}
}

func (x *xconn) internAtom(name string) (uint32, error) {
	body := make([]byte, 4, 4+len(name))
	le.PutUint16(body, uint16(len(name)))
	if err := x.request(16, 0, append(body, name...)); err != nil {
		return 0, err
	}
	r, err := x.reply()
	if err != nil {
		return 0, err
	}
	return le.Uint32(r[8:]), nil
}

// watchXFixes subscribes to selection-owner changes. The returned channel
// receives nil for each change, an error if the connection breaks, and is
// closed when it ends (ctx cancelled or the X server went away).
func watchXFixes(ctx context.Context, sel Selection) (<-chan error, error) {
	x, err := dialX11()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (<-chan error, error) { _ = x.c.Close(); return nil, err }

	_ = x.c.SetDeadline(time.Now().Add(3 * time.Second))
	atom := uint32(1) // XA_PRIMARY
	if sel == Clipboard {
		if atom, err = x.internAtom("CLIPBOARD"); err != nil {
			return fail(err)
		}
	}
	name := "XFIXES"
	body := make([]byte, 4, 4+len(name))
	le.PutUint16(body, uint16(len(name)))
	if err := x.request(98, 0, append(body, name...)); err != nil { // QueryExtension
		return fail(err)
	}
	r, err := x.reply()
	if err != nil {
		return fail(err)
	}
	if r[8] == 0 {
		return fail(errors.New("X server has no XFIXES extension"))
	}
	x.extOpcode, x.extEvent = r[9], r[10]

	ver := make([]byte, 8)
	le.PutUint32(ver, 5)
	if err := x.request(x.extOpcode, 0, ver); err != nil { // XFixesQueryVersion 5.0
		return fail(err)
	}
	if _, err := x.reply(); err != nil {
		return fail(err)
	}
	sub := make([]byte, 12)
	le.PutUint32(sub, x.root)
	le.PutUint32(sub[4:], atom)
	le.PutUint32(sub[8:], 1)                               // SetSelectionOwnerNotifyMask
	if err := x.request(x.extOpcode, 2, sub); err != nil { // XFixesSelectSelectionInput
		return fail(err)
	}
	_ = x.c.SetDeadline(time.Time{})

	out := make(chan error, 16)
	go func() {
		defer close(out)
		go func() { <-ctx.Done(); _ = x.c.Close() }()
		for {
			var b [32]byte
			if _, err := io.ReadFull(x.r, b[:]); err != nil {
				if ctx.Err() == nil {
					out <- err
				}
				return
			}
			t := b[0] & 0x7f
			if t == 35 || t == 1 { // GenericEvent / stray reply carry extra data
				if _, err := io.CopyN(io.Discard, x.r, 4*int64(le.Uint32(b[4:]))); err != nil {
					return
				}
				continue
			}
			if t == x.extEvent {
				select {
				case out <- nil:
				default: // a notification is already pending; coalesce
				}
			}
		}
	}()
	return out, nil
}
