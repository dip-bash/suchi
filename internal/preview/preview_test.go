package preview

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-sixel"
	"github.com/muesli/termenv"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

func TestDecodeSupportedFormats(t *testing.T) {
	src := solid(20, 10, color.NRGBA{200, 40, 40, 255})
	encoders := map[string]func(*bytes.Buffer) error{
		"png":  func(b *bytes.Buffer) error { return png.Encode(b, src) },
		"jpeg": func(b *bytes.Buffer) error { return jpeg.Encode(b, src, nil) },
		"gif":  func(b *bytes.Buffer) error { return gif.Encode(b, src, nil) },
		"bmp":  func(b *bytes.Buffer) error { return bmp.Encode(b, src) },
		"tiff": func(b *bytes.Buffer) error { return tiff.Encode(b, src, nil) },
	}
	for name, enc := range encoders {
		var b bytes.Buffer
		if err := enc(&b); err != nil {
			t.Fatal(err)
		}
		img, format, err := Decode(b.Bytes())
		if err != nil || format != name || img.Bounds().Dx() != 20 || img.Bounds().Dy() != 10 {
			t.Errorf("%s: format=%q err=%v bounds=%v", name, format, err, img.Bounds())
		}
		if w, h, f, err := Config(b.Bytes()); err != nil || w != 20 || h != 10 || f != name {
			t.Errorf("%s: Config = %d %d %q %v", name, w, h, f, err)
		}
	}
	if _, _, err := Decode([]byte("definitely not an image")); err == nil {
		t.Error("garbage must be rejected")
	}
}

// A tiny PNG whose header claims 60000x60000 pixels must not be decoded.
func TestDecodeRefusesDecompressionBombs(t *testing.T) {
	var b bytes.Buffer
	_ = png.Encode(&b, solid(2, 2, color.NRGBA{A: 255}))
	data := b.Bytes()
	binary.BigEndian.PutUint32(data[16:], 60000) // IHDR width
	binary.BigEndian.PutUint32(data[20:], 60000) // IHDR height
	binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
	if _, _, err := Decode(data); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want a too-large error, got %v", err)
	}
}

func TestFit(t *testing.T) {
	// a 200x100 image in a 40x20 box of 8x16px cells keeps its 2:1 pixel ratio
	cols, rows := Fit(200, 100, 40, 20, 8, 16)
	if got := float64(cols*8) / float64(rows*16); got < 1.8 || got > 2.2 {
		t.Errorf("aspect not kept: %dx%d cells -> pixel ratio %.2f", cols, rows, got)
	}
	if cols > 40 || rows > 20 {
		t.Errorf("outside the box: %dx%d", cols, rows)
	}
	// tall image is limited by rows
	if cols, rows := Fit(100, 2000, 40, 10, 8, 16); rows != 10 || cols >= 10 {
		t.Errorf("tall image: %dx%d", cols, rows)
	}
	// a tiny icon is not blown up to fill the box
	if cols, rows := Fit(16, 16, 80, 40, 1, 2); cols > 64 || rows > 32 {
		t.Errorf("upscaling must be capped, got %dx%d", cols, rows)
	}
	// degenerate input never panics and never returns zero
	for _, a := range [][6]int{{0, 0, 10, 10, 1, 2}, {10, 10, 0, 0, 1, 2}, {10, 10, 10, 10, 0, 0}} {
		if c, r := Fit(a[0], a[1], a[2], a[3], a[4], a[5]); c < 1 || r < 1 {
			t.Errorf("%v -> %dx%d", a, c, r)
		}
	}
}

func TestResizeCompositesTransparency(t *testing.T) {
	out := Resize(solid(4, 4, color.NRGBA{255, 0, 0, 0}), 2, 2) // fully transparent
	if c := out.NRGBAAt(0, 0); c.A != 255 || c.R > 0x40 {
		t.Fatalf("transparent pixels must show the dark background, got %+v", c)
	}
}

func TestRenderBlocksTrueColor(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })

	img := image.NewNRGBA(image.Rect(0, 0, 1, 2))
	img.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255}) // top: red
	img.SetNRGBA(0, 1, color.NRGBA{0, 0, 255, 255}) // bottom: blue
	out := RenderBlocks(img, 1, 1)
	if !strings.Contains(out, "▀") || !strings.Contains(out, "38;2;255;0;0") || !strings.Contains(out, "48;2;0;0;255") {
		t.Fatalf("top pixel must be the foreground and bottom pixel the background: %q", out)
	}
	big := RenderBlocks(solid(64, 64, color.NRGBA{10, 200, 10, 255}), 30, 8)
	if lines := strings.Split(big, "\n"); len(lines) != 8 {
		t.Fatalf("want 8 rows, got %d", len(lines))
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestRenderBlocksWithoutColourUsesABrightnessRamp(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	img := image.NewNRGBA(image.Rect(0, 0, 8, 2))
	for x := 0; x < 4; x++ { // left half black, right half white
		img.SetNRGBA(x, 0, color.NRGBA{0, 0, 0, 255})
		img.SetNRGBA(x, 1, color.NRGBA{0, 0, 0, 255})
		img.SetNRGBA(x+4, 0, color.NRGBA{255, 255, 255, 255})
		img.SetNRGBA(x+4, 1, color.NRGBA{255, 255, 255, 255})
	}
	out := RenderBlocks(img, 8, 1)
	if ansiRe.MatchString(out) || len(out) != 8 || out[0] >= out[7] && out[0] != ' ' {
		t.Fatalf("got %q", out)
	}
	if out[0] != ' ' || out[7] != '@' {
		t.Fatalf("dark must map to a space and bright to @, got %q", out)
	}
}

func TestKittySequenceRoundTrip(t *testing.T) {
	var b bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7) // noisy, so the PNG is bigger than one 4 KB chunk
	}
	_ = png.Encode(&b, img)
	seq := KittySequence(b.Bytes(), 30, 15)

	chunks := strings.Split(strings.TrimSuffix(seq, "\x1b\\"), "\x1b\\")
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	var payload strings.Builder
	for i, c := range chunks {
		head, data, ok := strings.Cut(strings.TrimPrefix(c, "\x1b_G"), ";")
		if !ok || len(data) > 4096 {
			t.Fatalf("chunk %d malformed (data %d bytes)", i, len(data))
		}
		last := i == len(chunks)-1
		if last != strings.Contains(head, "m=0") || (!last && !strings.Contains(head, "m=1")) {
			t.Errorf("chunk %d: more-flag wrong: %q", i, head)
		}
		if i == 0 && (!strings.Contains(head, "a=T") || !strings.Contains(head, "f=100") ||
			!strings.Contains(head, "c=30") || !strings.Contains(head, "r=15") || !strings.Contains(head, "C=1")) {
			t.Errorf("first chunk header: %q", head)
		}
		payload.WriteString(data)
	}
	got, err := base64.StdEncoding.DecodeString(payload.String())
	if err != nil || !bytes.Equal(got, b.Bytes()) {
		t.Fatalf("reassembled payload differs from the PNG (err=%v)", err)
	}
	if KittyDelete() != "\x1b_Ga=d,d=A,q=2\x1b\\" {
		t.Error("delete sequence changed")
	}
}

func TestITerm2Sequence(t *testing.T) {
	seq := ITerm2Sequence([]byte("PNGBYTES"), 20, 10)
	if !strings.HasPrefix(seq, "\x1b]1337;File=inline=1;size=8;width=20;height=10;preserveAspectRatio=1:") || !strings.HasSuffix(seq, "\a") {
		t.Fatalf("got %q", seq)
	}
	b64 := seq[strings.Index(seq, ":")+1 : len(seq)-1]
	if got, _ := base64.StdEncoding.DecodeString(b64); string(got) != "PNGBYTES" {
		t.Fatalf("payload %q", got)
	}
}

func TestSixelRoundTrip(t *testing.T) {
	src := solid(40, 40, color.NRGBA{220, 30, 30, 255})
	seq, err := Graphic(Sixel, src, 4, 3, 8, 12) // 32 x 36 pixels
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seq, "\x1bP") || !strings.HasSuffix(seq, "\x1b\\") {
		t.Fatalf("not a DCS sixel sequence: %q...", seq[:min(20, len(seq))])
	}
	var out image.Image
	if err := sixel.NewDecoder(strings.NewReader(seq)).Decode(&out); err != nil {
		t.Fatalf("our sixel output does not decode: %v", err)
	}
	if out.Bounds().Dx() != 32 || out.Bounds().Dy() < 36 {
		t.Fatalf("decoded size %v, want 32x36", out.Bounds())
	}
	r, g, b, _ := out.At(10, 10).RGBA()
	if r>>8 < 180 || g>>8 > 80 || b>>8 > 80 {
		t.Fatalf("colour lost in the round trip: %d %d %d", r>>8, g>>8, b>>8)
	}
}

func TestGraphicRejectsNonGraphicProtocols(t *testing.T) {
	if _, err := Graphic(Blocks, solid(2, 2, color.NRGBA{A: 255}), 1, 1, 8, 16); err == nil {
		t.Fatal("blocks is not a graphics protocol")
	}
	if !Kitty.IsGraphic() || !Sixel.IsGraphic() || !ITerm2.IsGraphic() || Blocks.IsGraphic() || None.IsGraphic() {
		t.Fatal("IsGraphic classification wrong")
	}
	if got := Place("IMG", 5, 7); got != "\x1b7\x1b[5;7HIMG\x1b8" {
		t.Fatalf("got %q", got)
	}
}

func TestDetect(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name string
		env  func(string) string
		want Protocol
	}{
		{"plain xterm", env("TERM", "xterm-256color"), Blocks},
		{"kitty", env("TERM", "xterm-kitty", "KITTY_WINDOW_ID", "3"), Kitty},
		{"ghostty", env("TERM", "xterm-ghostty", "TERM_PROGRAM", "ghostty"), Kitty},
		{"iterm2", env("TERM", "xterm-256color", "TERM_PROGRAM", "iTerm.app"), ITerm2},
		{"wezterm", env("TERM", "xterm-256color", "TERM_PROGRAM", "WezTerm"), ITerm2},
		{"foot", env("TERM", "foot"), Sixel},
		{"konsole", env("TERM", "xterm-256color", "KONSOLE_VERSION", "230800"), Sixel},
		{"linux console", env("TERM", "linux"), Blocks},
		{"tmux inside kitty", env("TERM", "tmux-256color", "TMUX", "/tmp/tmux", "KITTY_WINDOW_ID", "3"), Blocks},
		{"override wins", env("TERM", "xterm-256color", "SUCHI_IMAGE", "Sixel"), Sixel},
		{"override none", env("TERM", "xterm-kitty", "SUCHI_IMAGE", "none"), None},
		{"bad override ignored", env("TERM", "xterm-kitty", "KITTY_WINDOW_ID", "1", "SUCHI_IMAGE", "bogus"), Kitty},
	}
	for _, c := range cases {
		if got := Detect(c.env); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	for path, want := range map[string]struct {
		label string
		cat   Category
	}{
		"/a/demo.MP4": {"MP4", CatVideo}, "/a/shot.png": {"PNG", CatImage}, "/a/x.tar": {"TAR", CatArchive},
		"/a/main.go": {"GO", CatCode}, "/a/song.flac": {"FLAC", CatAudio}, "/a/doc.pdf": {"PDF", CatDoc},
		"/a/notes.txt": {"TXT", CatText}, "/a/Makefile": {"FILE", CatOther}, "/a/x.weirdext": {"WEIRD", CatOther},
	} {
		if l, c := Classify(path, false); l != want.label || c != want.cat {
			t.Errorf("%s -> %s %d, want %s %d", path, l, c, want.label, want.cat)
		}
	}
	if l, c := Classify("/a/dir", true); l != "DIR" || c != CatFolder {
		t.Errorf("dir -> %s %d", l, c)
	}
	for mime, want := range map[string]string{"image/jpeg": "JPG", "image/png": "PNG", "image/x-ms-bmp": "BMP", "image/webp": "WEBP", "": "IMG"} {
		if got := LabelForMime(mime); got != want {
			t.Errorf("LabelForMime(%q) = %q, want %q", mime, got, want)
		}
	}
	if !DecodableImage("/x/a.JPG") || DecodableImage("/x/a.svg") || DecodableImage("/x/a.mp4") {
		t.Error("DecodableImage wrong")
	}
}

func TestHumanSizeAndShortPath(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 5 << 20: "5.0 MB", 3 << 30: "3.0 GB"} {
		if got := HumanSize(n); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", n, got, want)
		}
	}
	t.Setenv("HOME", "/home/dip")
	if got := ShortPath("/home/dip/Videos/demo.mp4", 80); got != "~/Videos/demo.mp4" {
		t.Errorf("home abbreviation: %q", got)
	}
	got := ShortPath("/very/long/directory/structure/with/many/parts/demo.mp4", 24)
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "demo.mp4") || len([]rune(got)) > 24 {
		t.Errorf("long path: %q", got)
	}
}

func TestStat(t *testing.T) {
	dir := t.TempDir()
	if in := Stat(dir); !in.Exists || !in.IsDir || in.Label != "DIR" {
		t.Errorf("dir: %+v", in)
	}
	if in := Stat(dir + "/missing.mp4"); in.Exists || in.Label != "MP4" || in.Name != "missing.mp4" {
		t.Errorf("missing file must still be classified: %+v", in)
	}
}
