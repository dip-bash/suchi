package clipboard

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeCompositor is a minimal Wayland server that advertises wl_seat and the
// ext-data-control manager, and offers one selection.
type fakeCompositor struct {
	t     *testing.T
	text  string
	mimes []string

	askMime  string // mime requested from a source set by the client
	mu       sync.Mutex
	pasted   chan string // text received through a source's send() event
	setCalls int
}

const fakeOfferID = 0xff000000

func startFake(t *testing.T, text string, mimes ...string) *fakeCompositor {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("WAYLAND_DISPLAY", "wl-test")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "wl-test"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCompositor{t: t, text: text, mimes: mimes, askMime: "text/plain;charset=utf-8", pasted: make(chan string, 4)}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeCompositor) serve(c *net.UnixConn) {
	defer c.Close()
	send := func(obj uint32, op uint16, payload []byte, fds ...int) {
		msg := make([]byte, 8, 8+len(payload))
		native.PutUint32(msg, obj)
		native.PutUint32(msg[4:], uint32(8+len(payload))<<16|uint32(op))
		msg = append(msg, payload...)
		var oob []byte
		if len(fds) > 0 {
			oob = syscall.UnixRights(fds...)
		}
		_, _, _ = c.WriteMsgUnix(msg, oob, nil)
	}
	var registry, dev, source uint32
	objects := map[uint32]string{}
	var rbuf []byte
	var fds []int
	buf, oob := make([]byte, 4096), make([]byte, syscall.CmsgSpace(16))
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		if oobn > 0 {
			if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
				for i := range msgs {
					if got, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
						fds = append(fds, got...)
					}
				}
			}
		}
		rbuf = append(rbuf, buf[:n]...)
		for len(rbuf) >= 8 {
			obj := native.Uint32(rbuf)
			so := native.Uint32(rbuf[4:])
			size, op := int(so>>16), uint16(so&0xffff)
			if len(rbuf) < size {
				break
			}
			p := append([]byte(nil), rbuf[8:size]...)
			rbuf = rbuf[size:]
			switch {
			case obj == 1 && op == 1: // get_registry
				registry = native.Uint32(p)
				send(registry, 0, append(append(u32(1), wlString("wl_seat")...), u32(7)...))
				send(registry, 0, append(append(u32(2), wlString(mgrExt)...), u32(1)...))
			case obj == 1 && op == 0: // sync
				send(native.Uint32(p), 0, u32(0))
			case obj == registry && op == 0: // bind
				_, off, _ := readWLString(p, 4)
				id := native.Uint32(p[off+4:])
				iface, _, _ := readWLString(p, 4)
				objects[id] = iface
			case objects[obj] == mgrExt && op == 1: // get_data_device
				dev = native.Uint32(p)
				send(dev, 0, u32(fakeOfferID)) // data_offer
				for _, m := range f.mimes {
					send(fakeOfferID, 0, wlString(m)) // offer.offer
				}
				send(dev, 1, u32(fakeOfferID)) // selection
			case obj == fakeOfferID && op == 0: // receive(mime, fd)
				if len(fds) > 0 {
					fd := fds[0]
					fds = fds[1:]
					file := os.NewFile(uintptr(fd), "paste")
					_, _ = file.WriteString(f.text)
					_ = file.Close()
				}
			case objects[obj] == mgrExt && op == 0: // create_data_source
				source = native.Uint32(p)
			case obj == dev && op == 0 && dev != 0: // set_selection: ask the client for the data
				f.mu.Lock()
				f.setCalls++
				f.mu.Unlock()
				r, w, _ := os.Pipe()
				f.mu.Lock()
				ask := f.askMime
				f.mu.Unlock()
				send(source, 0, wlString(ask), int(w.Fd()))
				_ = w.Close()
				go func() {
					buf := make([]byte, 1<<20)
					total := 0
					_ = r.SetReadDeadline(time.Now().Add(3 * time.Second))
					for {
						n, err := r.Read(buf[total:])
						total += n
						if err != nil {
							break
						}
					}
					_ = r.Close()
					f.pasted <- string(buf[:total])
				}()
			}
		}
	}
}

func watchOnce(t *testing.T, lim Limits) (Item, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var got Item
	ok := false
	err := Wayland{}.Watch(ctx, Clipboard, lim, func(it Item) { got, ok = it, true })
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	return got, ok
}

var bigLimits = Limits{MaxText: 1 << 20, MaxMedia: 1 << 20}

func TestWaylandWatchReadsTextSelection(t *testing.T) {
	startFake(t, "héllo from the compositor", "image/png", "text/plain;charset=utf-8")
	got, ok := watchOnce(t, bigLimits)
	if !ok || got.Kind != KindText || got.Text != "héllo from the compositor" {
		t.Fatalf("text must win over the picture of it: %+v ok=%v", got, ok)
	}
}

func TestWaylandWatchReadsImages(t *testing.T) {
	startFake(t, "\x89PNG-bytes", "image/png")
	got, ok := watchOnce(t, bigLimits)
	if !ok || got.Kind != KindImage || got.MIME != "image/png" || string(got.Data) != "\x89PNG-bytes" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	startFake(t, "jpegbytes", "image/bmp", "image/jpeg")
	if got, _ := watchOnce(t, bigLimits); got.MIME != "image/jpeg" {
		t.Fatalf("jpeg must be preferred to bmp, got %q", got.MIME)
	}
}

func TestWaylandWatchReadsCopiedFiles(t *testing.T) {
	startFake(t, "# copied in a file manager\r\nfile:///home/u/My%20Videos/demo.mp4\r\nfile:///home/u/a.png\r\n",
		"text/uri-list", "text/plain")
	got, ok := watchOnce(t, bigLimits)
	if !ok || got.Kind != KindFiles || got.Text != "/home/u/My Videos/demo.mp4\n/home/u/a.png" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestWebLinkListIsReadAsText(t *testing.T) {
	// a browser offers text/uri-list for a link: that is not a file copy
	startFake(t, "https://example.com/page", "text/uri-list", "text/plain")
	got, ok := watchOnce(t, bigLimits)
	if !ok || got.Kind != KindText || got.Text != "https://example.com/page" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestWaylandWatchSkipsSecretsAndOversize(t *testing.T) {
	startFake(t, "hunter2", "text/plain", "x-kde-passwordManagerHint")
	if got, ok := watchOnce(t, bigLimits); ok {
		t.Fatalf("password-manager entry must be skipped, got %+v", got)
	}
	startFake(t, "0123456789", "text/plain")
	if got, ok := watchOnce(t, Limits{MaxText: 5, MaxMedia: 5}); ok {
		t.Fatalf("oversized text must be skipped, got %+v", got)
	}
	startFake(t, "0123456789", "image/png")
	if got, ok := watchOnce(t, Limits{MaxText: 1 << 20, MaxMedia: 5}); ok {
		t.Fatalf("oversized image must be skipped, got %+v", got)
	}
	startFake(t, "<svg/>", "image/svg+xml")
	if got, ok := watchOnce(t, bigLimits); ok {
		t.Fatalf("unsupported image types are skipped, got %+v", got)
	}
}

func TestWaylandSetAnswersPasteRequests(t *testing.T) {
	f := startFake(t, "", "text/plain")
	w, err := dialWayland()
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	want := "pasted by another client ✓"
	if err := w.setSelection(Item{Kind: KindText, Text: want}, Clipboard); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.serve(ctx) }()
	select {
	case got := <-f.pasted:
		if got != want {
			t.Fatalf("compositor received %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no paste data arrived")
	}
}

func TestWaylandSetOffersImagesAndFiles(t *testing.T) {
	f := startFake(t, "", "text/plain")
	w, err := dialWayland()
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	f.mu.Lock()
	f.askMime = "image/png"
	f.mu.Unlock()
	if err := w.setSelection(Item{Kind: KindImage, MIME: "image/png", Data: []byte("PNGDATA")}, Clipboard); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.serve(ctx) }()
	select {
	case got := <-f.pasted:
		if got != "PNGDATA" {
			t.Fatalf("image paste got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no paste data arrived")
	}
}

func TestDetect(t *testing.T) {
	t.Run("no display falls back to tty", func(t *testing.T) {
		t.Setenv("WAYLAND_DISPLAY", "")
		t.Setenv("DISPLAY", "")
		b, problem, err := Detect("auto")
		if err != nil || b.Name() != "tty" || problem != "" {
			t.Fatalf("got %v %q %v", b, problem, err)
		}
	})
	t.Run("wayland is preferred when it works", func(t *testing.T) {
		startFake(t, "x", "text/plain")
		if b, _, err := Detect("auto"); err != nil || b.Name() != "wayland" {
			t.Fatalf("got %v %v", b, err)
		}
	})
	t.Run("forcing an unusable backend is an error", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		t.Setenv("WAYLAND_DISPLAY", "nope")
		if _, _, err := Detect("wayland"); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("a dead wayland socket reports the problem and falls back", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		t.Setenv("WAYLAND_DISPLAY", "nope")
		t.Setenv("DISPLAY", "")
		b, problem, err := Detect("auto")
		if err != nil || b.Name() != "tty" || problem == "" {
			t.Fatalf("got %v %q %v", b, problem, err)
		}
	})
}

type failing struct{ TTY }

func (failing) Name() string { return "wayland" }
func (failing) Set(context.Context, Item, Selection) error {
	return errors.New("compositor said no")
}

func TestDeliverNeverLosesTheChoice(t *testing.T) {
	t.Setenv("TERM", "") // no OSC 52, no console injection
	var out, errw bytes.Buffer
	if err := Deliver(context.Background(), failing{}, "", Item{Kind: KindText, Text: "the text"}, Clipboard, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if out.String() != "the text" {
		t.Fatalf("last resort must print the text, got %q", out.String())
	}
	if !bytes.Contains(errw.Bytes(), []byte("compositor said no")) {
		t.Fatalf("the failure must be reported, got %q", errw.String())
	}
}

func TestSanitizeForTerminal(t *testing.T) {
	got := SanitizeForTerminal("a\x1b[31mred\x1b[0m\tb\nc\u009bd")
	if got != "a·[31mred·[0m\tb\nc·d" {
		t.Fatalf("got %q", got)
	}
}

func TestInjectableOnlyAllowsOneCleanLine(t *testing.T) {
	for s, want := range map[string]bool{
		"ls -la": true, "echo héllo": true, "": false,
		"a\nb": false, "a\rb": false, "\x1b[2J": false, "tab\there": false, "zero\u200bwidth": false,
	} {
		if got := injectable(s); got != want {
			t.Errorf("injectable(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestDisplayNumber(t *testing.T) {
	for in, want := range map[string]string{":0": "0", ":1.0": "1", "unix:12": "12"} {
		if got, err := displayNumber(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	if _, err := displayNumber("remote:0"); err == nil {
		t.Error("tcp displays are not supported and must say so")
	}
}

func TestXauthorityCookie(t *testing.T) {
	host, _ := os.Hostname()
	entry := func(family uint16, addr, number, name string, data []byte) []byte {
		b := binary.BigEndian.AppendUint16(nil, family)
		for _, f := range [][]byte{[]byte(addr), []byte(number), []byte(name), data} {
			b = binary.BigEndian.AppendUint16(b, uint16(len(f)))
			b = append(b, f...)
		}
		return b
	}
	var file []byte
	file = append(file, entry(256, "otherhost", "0", "MIT-MAGIC-COOKIE-1", []byte("wrong-host"))...)
	file = append(file, entry(256, host, "1", "MIT-MAGIC-COOKIE-1", []byte("wrong-display"))...)
	file = append(file, entry(256, host, "0", "XDM-AUTHORIZATION-1", []byte("wrong-scheme"))...)
	file = append(file, entry(256, host, "0", "MIT-MAGIC-COOKIE-1", []byte("right-cookie"))...)
	p := filepath.Join(t.TempDir(), "Xauthority")
	if err := os.WriteFile(p, file, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XAUTHORITY", p)
	if got := cookie("0"); string(got) != "right-cookie" {
		t.Fatalf("got %q", got)
	}
	if got := cookie("7"); got != nil {
		t.Fatalf("unknown display must yield no cookie, got %q", got)
	}
}

func TestWaylandStringRoundTrip(t *testing.T) {
	for _, s := range []string{"", "a", "text/plain;charset=utf-8", "héllo ✓"} {
		b := wlString(s)
		if len(b)%4 != 0 {
			t.Fatalf("%q not 4-byte aligned", s)
		}
		got, off, err := readWLString(b, 0)
		if err != nil || got != s || off != len(b) {
			t.Fatalf("%q: got %q off=%d len=%d err=%v", s, got, off, len(b), err)
		}
	}
	if _, _, err := readWLString([]byte{9, 0, 0, 0, 'a'}, 0); err == nil {
		t.Fatal("truncated string must be an error")
	}
}

func TestDeliverRefusesImagesWithoutAClipboard(t *testing.T) {
	t.Setenv("TERM", "")
	var out, errw bytes.Buffer
	err := Deliver(context.Background(), TTY{}, "", Item{Kind: KindImage, MIME: "image/png", Data: []byte("x")}, Clipboard, &out, &errw)
	if err == nil || out.Len() != 0 {
		t.Fatalf("an image must not be dumped into a terminal: err=%v out=%q", err, out.String())
	}
}

func TestParseFileURIs(t *testing.T) {
	in := "# comment\r\nfile:///a/b%20c/d.txt\nfile://localhost/x/y.png\nhttps://example.com/\nfile://otherhost/nope\ncopy\nnot a uri\n"
	want := []string{"/a/b c/d.txt", "/x/y.png"}
	got := ParseFileURIs(in)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %q", got)
	}
	if got := ParseFileURIs("copy\nfile:///only/one"); len(got) != 1 || got[0] != "/only/one" {
		t.Fatalf("gnome-copied-files format: %q", got)
	}
}

func TestFileURIRoundTrip(t *testing.T) {
	for _, p := range []string{"/plain/path.txt", "/with space/é ü/日本語 (1).mp4", "/percent%sign/#hash?x"} {
		if got := ParseFileURIs(FileURI(p)); len(got) != 1 || got[0] != p {
			t.Errorf("%q -> %q -> %q", p, FileURI(p), got)
		}
	}
}

func TestCandidatesRanking(t *testing.T) {
	cases := []struct {
		mimes []string
		want  []candidate
	}{
		{[]string{"image/png"}, []candidate{{KindImage, "image/png"}}},
		{[]string{"text/plain", "image/png"}, []candidate{{KindText, "text/plain"}, {KindImage, "image/png"}}},
		{[]string{"text/uri-list", "text/plain;charset=utf-8"}, []candidate{{KindFiles, "text/uri-list"}, {KindText, "text/plain;charset=utf-8"}}},
		{[]string{"x-special/gnome-copied-files", "UTF8_STRING"}, []candidate{{KindFiles, "x-special/gnome-copied-files"}, {KindText, "UTF8_STRING"}}},
		{[]string{"TARGETS", "TIMESTAMP"}, nil},
		{[]string{"text/plain", "x-kde-passwordManagerHint"}, nil},
	}
	for _, c := range cases {
		got := candidates(c.mimes)
		if len(got) != len(c.want) {
			t.Errorf("%v: got %v want %v", c.mimes, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%v: got %v want %v", c.mimes, got, c.want)
			}
		}
	}
}

func TestFormatsForFiles(t *testing.T) {
	fs := Formats(Item{Kind: KindFiles, Text: "/a b/c.txt\n/d.png"})
	m := map[string]string{}
	for _, f := range fs {
		m[f.MIME] = string(f.Data)
	}
	if m["text/uri-list"] != "file:///a%20b/c.txt\r\nfile:///d.png\r\n" ||
		m["x-special/gnome-copied-files"] != "copy\nfile:///a%20b/c.txt\nfile:///d.png" ||
		m["text/plain"] != "/a b/c.txt\n/d.png" {
		t.Fatalf("got %q", m)
	}
}

func TestItemFramingRoundTrip(t *testing.T) {
	for _, it := range []Item{
		{Kind: KindText, Text: "line1\nline2 ✓"},
		{Kind: KindFiles, MIME: "text/uri-list", Text: "/a/b\n/c/d"},
		{Kind: KindImage, MIME: "image/png", Data: []byte{0, 1, 2, '\n', 255}},
	} {
		var buf bytes.Buffer
		if err := encodeItem(&buf, it); err != nil {
			t.Fatal(err)
		}
		got, err := decodeItem(&buf)
		if err != nil || got.Kind != it.Kind || got.Text != it.Text || got.MIME != it.MIME || !bytes.Equal(got.Data, it.Data) {
			t.Fatalf("%+v -> %+v (%v)", it, got, err)
		}
	}
	if _, err := decodeItem(bytes.NewReader([]byte("not json\nbody"))); err == nil {
		t.Fatal("garbage must be rejected")
	}
}
