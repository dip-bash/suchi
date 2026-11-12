package clipboard

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Wayland is the native Wayland backend. It speaks just enough of the wire
// protocol to use the data-control extension (ext-data-control-v1 or
// wlr-data-control-unstable-v1), which lets a background client read and set
// the clipboard without owning a window.
type Wayland struct{}

// Name implements Backend.
func (Wayland) Name() string { return "wayland" }

const (
	mgrExt = "ext_data_control_manager_v1"
	mgrWlr = "zwlr_data_control_manager_v1"
)

var (
	textMimes   = []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "TEXT", "STRING"}
	secretMimes = []string{"x-kde-passwordManagerHint"} // password managers flag secrets with this
)

var native = binary.NativeEndian

type wlGlobal struct{ name, version uint32 }

type wlSelEvent struct {
	kind Selection
	id   uint32
}

type wlSend struct {
	mime string
	fd   int
}

// wlConn is one Wayland connection with the data-control objects bound.
type wlConn struct {
	conn *net.UnixConn
	buf  []byte
	oob  []byte
	rbuf []byte
	fds  []int

	nextID                       uint32
	registry, mgr, dev, source   uint32
	globals                      map[string]wlGlobal
	offers                       map[uint32][]string
	current                      map[Selection]uint32
	events                       []wlSelEvent
	sends                        []wlSend
	callbacks                    map[uint32]bool
	hasPrimary, cancelled, ended bool
	formats                      map[string][]byte
}

func probeWayland() error {
	w, err := dialWayland()
	if err != nil {
		return err
	}
	w.close()
	return nil
}

func dialWayland() (*wlConn, error) {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		name = "wayland-0"
	}
	path := name
	if !filepath.IsAbs(name) {
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			return nil, errors.New("XDG_RUNTIME_DIR is not set; cannot find the Wayland socket")
		}
		path = filepath.Join(rt, name)
	}
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to Wayland socket %s: %w", path, err)
	}
	w := &wlConn{
		conn:      c.(*net.UnixConn),
		buf:       make([]byte, 64*1024),
		oob:       make([]byte, syscall.CmsgSpace(4*32)),
		nextID:    2,
		globals:   map[string]wlGlobal{},
		offers:    map[uint32][]string{},
		current:   map[Selection]uint32{},
		callbacks: map[uint32]bool{},
	}
	if err := w.setup(); err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}

func (w *wlConn) close() { _ = w.conn.Close() }

func (w *wlConn) newID() uint32 { w.nextID++; return w.nextID - 1 }

func wlString(s string) []byte {
	n := len(s) + 1
	b := make([]byte, 4, 4+n+3)
	native.PutUint32(b, uint32(n))
	b = append(b, s...)
	b = append(b, 0)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func readWLString(p []byte, off int) (string, int, error) {
	if off+4 > len(p) {
		return "", 0, errors.New("short string")
	}
	n := int(native.Uint32(p[off:]))
	end := off + 4 + n
	if n < 0 || end > len(p) {
		return "", 0, errors.New("short string")
	}
	s := ""
	if n > 0 {
		s = string(p[off+4 : end-1])
	}
	return s, off + 4 + (n+3)&^3, nil
}

func u32(vs ...uint32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		native.PutUint32(b[4*i:], v)
	}
	return b
}

// send writes one request; fds are passed as SCM_RIGHTS ancillary data.
func (w *wlConn) send(obj uint32, op uint16, payload []byte, fds ...int) error {
	msg := make([]byte, 8, 8+len(payload))
	native.PutUint32(msg, obj)
	native.PutUint32(msg[4:], uint32(8+len(payload))<<16|uint32(op))
	msg = append(msg, payload...)
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	_, _, err := w.conn.WriteMsgUnix(msg, oob, nil)
	if err != nil {
		return fmt.Errorf("wayland connection lost: %w", err)
	}
	return nil
}

// fill waits up to timeout for data and dispatches whatever arrived.
func (w *wlConn) fill(timeout time.Duration) (bool, error) {
	_ = w.conn.SetReadDeadline(time.Now().Add(timeout))
	n, oobn, _, _, err := w.conn.ReadMsgUnix(w.buf, w.oob)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return false, nil
		}
		if errors.Is(err, io.EOF) {
			return false, errors.New("wayland compositor closed the connection")
		}
		return false, fmt.Errorf("wayland connection lost: %w", err)
	}
	if n == 0 && oobn == 0 {
		return false, errors.New("wayland compositor closed the connection")
	}
	if oobn > 0 {
		msgs, err := syscall.ParseSocketControlMessage(w.oob[:oobn])
		if err == nil {
			for i := range msgs {
				if fds, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
					w.fds = append(w.fds, fds...)
				}
			}
		}
	}
	w.rbuf = append(w.rbuf, w.buf[:n]...)
	for len(w.rbuf) >= 8 {
		obj := native.Uint32(w.rbuf)
		so := native.Uint32(w.rbuf[4:])
		size, op := int(so>>16), uint16(so&0xffff)
		if size < 8 {
			return false, errors.New("wayland protocol error: malformed message")
		}
		if len(w.rbuf) < size {
			break
		}
		payload := append([]byte(nil), w.rbuf[8:size]...)
		w.rbuf = w.rbuf[size:]
		if err := w.dispatch(obj, op, payload); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (w *wlConn) roundtrip() error {
	cb := w.newID()
	w.callbacks[cb] = true
	if err := w.send(1, 0, u32(cb)); err != nil { // wl_display.sync
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for w.callbacks[cb] {
		if time.Now().After(deadline) {
			return errors.New("wayland compositor is not responding")
		}
		if _, err := w.fill(500 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (w *wlConn) dispatch(obj uint32, op uint16, p []byte) error {
	switch {
	case obj == 1: // wl_display
		if op == 0 && len(p) >= 8 { // error
			msg, _, _ := readWLString(p, 8)
			return fmt.Errorf("wayland protocol error %d on object %d: %s",
				native.Uint32(p[4:]), native.Uint32(p), msg)
		}
	case w.callbacks[obj]:
		delete(w.callbacks, obj)
	case obj == w.registry:
		if op == 0 && len(p) >= 8 { // global
			iface, off, err := readWLString(p, 4)
			if err == nil && off+4 <= len(p) {
				if _, dup := w.globals[iface]; !dup {
					w.globals[iface] = wlGlobal{native.Uint32(p), native.Uint32(p[off:])}
				}
			}
		}
	case obj == w.dev && obj != 0:
		if len(p) < 4 && op != 2 {
			return nil
		}
		switch op {
		case 0: // data_offer(new_id)
			w.offers[native.Uint32(p)] = nil
		case 1:
			w.onSelection(Clipboard, native.Uint32(p))
		case 3:
			w.onSelection(Primary, native.Uint32(p))
		case 2: // finished
			w.ended = true
		}
	case obj == w.source && obj != 0:
		switch op {
		case 0: // send(mime, fd)
			mime, _, _ := readWLString(p, 0)
			if len(w.fds) > 0 {
				w.sends = append(w.sends, wlSend{mime, w.fds[0]})
				w.fds = w.fds[1:]
			}
		case 1: // cancelled
			w.cancelled = true
		}
	default:
		if mimes, ok := w.offers[obj]; ok && op == 0 { // offer(mime)
			if mime, _, err := readWLString(p, 0); err == nil {
				w.offers[obj] = append(mimes, mime)
			}
		}
		// events for unknown or already destroyed objects are ignored
	}
	return nil
}

func (w *wlConn) onSelection(kind Selection, id uint32) {
	if old := w.current[kind]; old != 0 && old != id {
		if _, ok := w.offers[old]; ok {
			_ = w.send(old, 1, nil) // offer.destroy
			delete(w.offers, old)
		}
	}
	w.current[kind] = id
	w.events = append(w.events, wlSelEvent{kind, id})
}

func (w *wlConn) bind(iface string, version uint32) (uint32, uint32, error) {
	g := w.globals[iface]
	v := min(version, g.version)
	id := w.newID()
	payload := append(u32(g.name), wlString(iface)...)
	payload = append(payload, u32(v, id)...)
	return id, v, w.send(w.registry, 0, payload)
}

func (w *wlConn) setup() error {
	w.registry = w.newID()
	if err := w.send(1, 1, u32(w.registry)); err != nil { // wl_display.get_registry
		return err
	}
	if err := w.roundtrip(); err != nil {
		return err
	}
	iface := ""
	for _, m := range []string{mgrExt, mgrWlr} {
		if _, ok := w.globals[m]; ok {
			iface = m
			break
		}
	}
	if _, seat := w.globals["wl_seat"]; iface == "" || !seat {
		return errors.New("this Wayland compositor has no background clipboard protocol " +
			"(ext-data-control / wlr-data-control); GNOME/Mutter is like this - " +
			"suchi uses XWayland (x11 backend) instead when available")
	}
	seat, _, err := w.bind("wl_seat", 1)
	if err != nil {
		return err
	}
	var ver uint32
	if w.mgr, ver, err = w.bind(iface, 2); err != nil {
		return err
	}
	w.hasPrimary = iface == mgrExt || ver >= 2
	w.dev = w.newID()
	if err := w.send(w.mgr, 1, u32(w.dev, seat)); err != nil { // get_data_device
		return err
	}
	return w.roundtrip() // delivers the current selection events
}

// readMime reads one representation of an offer through a pipe.
func (w *wlConn) readMime(id uint32, mime string, maxBytes int) ([]byte, bool) {
	r, wr, err := os.Pipe()
	if err != nil {
		return nil, false
	}
	defer r.Close()
	err = w.send(id, 0, wlString(mime), int(wr.Fd())) // offer.receive
	_ = wr.Close()
	if err != nil {
		return nil, false
	}
	_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil || len(data) > maxBytes {
		return nil, false
	}
	return data, true
}

// readOffer turns a selection offer into an Item, or reports false when it
// is flagged secret, unsupported, too big, or the owner does not answer.
func (w *wlConn) readOffer(id uint32, lim Limits) (Item, bool) {
	for _, c := range candidates(w.offers[id]) {
		data, ok := w.readMime(id, c.mime, maxFor(c, lim))
		if !ok {
			return Item{}, false
		}
		if it, ok := buildItem(c, data, lim); ok {
			return it, true
		} else if c.kind != KindFiles {
			return Item{}, false
		}
		// a uri-list of web links: fall through to the text representation
	}
	return Item{}, false
}

func (w *wlConn) deliver(kind Selection, lim Limits, fn func(Item)) {
	var last *wlSelEvent
	for i := range w.events {
		if w.events[i].kind == kind {
			last = &w.events[i]
		}
	}
	w.events = w.events[:0]
	if last == nil || last.id == 0 {
		return
	}
	if it, ok := w.readOffer(last.id, lim); ok {
		fn(it)
	}
}

// Watch implements Backend.
func (Wayland) Watch(ctx context.Context, sel Selection, lim Limits, fn func(Item)) error {
	w, err := dialWayland()
	if err != nil {
		return err
	}
	defer w.close()
	if sel == Primary && !w.hasPrimary {
		return errors.New("this compositor does not support the primary selection")
	}
	w.deliver(sel, lim, fn)
	for ctx.Err() == nil {
		if w.ended {
			return errors.New("wayland seat went away")
		}
		if _, err := w.fill(500 * time.Millisecond); err != nil {
			return err
		}
		w.deliver(sel, lim, fn)
	}
	return nil
}

// setSelection offers text as the selection; serve must be called to answer pastes.
func (w *wlConn) setSelection(it Item, sel Selection) error {
	if sel == Primary && !w.hasPrimary {
		return errors.New("this compositor does not support the primary selection")
	}
	w.source = w.newID()
	if err := w.send(w.mgr, 0, u32(w.source)); err != nil { // create_data_source
		return err
	}
	w.formats = map[string][]byte{}
	for _, f := range Formats(it) {
		w.formats[f.MIME] = f.Data
		if err := w.send(w.source, 0, wlString(f.MIME)); err != nil { // source.offer
			return err
		}
	}
	op := uint16(0)
	if sel == Primary {
		op = 2
	}
	if err := w.send(w.dev, op, u32(w.source)); err != nil {
		return err
	}
	if err := w.roundtrip(); err != nil { // surfaces protocol errors synchronously
		return err
	}
	if w.cancelled {
		return errors.New("compositor refused the selection")
	}
	return nil
}

// serve answers paste requests until another client takes over the selection.
func (w *wlConn) serve(ctx context.Context) error {
	for ctx.Err() == nil && !w.cancelled && !w.ended {
		if _, err := w.fill(500 * time.Millisecond); err != nil {
			return err
		}
		for _, s := range w.sends {
			go func(fd int, data []byte) {
				f := os.NewFile(uintptr(fd), "wl-paste")
				defer f.Close()
				_, _ = f.Write(data)
			}(s.fd, w.formats[s.mime])
		}
		w.sends = w.sends[:0]
	}
	return nil
}
