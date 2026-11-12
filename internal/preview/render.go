package preview

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"strings"
	"syscall"
	"unsafe"

	"github.com/NimbleMarkets/ntcharts/canvas"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-sixel"
	"github.com/muesli/termenv"
)

// Protocol is how an image is put on screen.
type Protocol string

// Supported protocols.
const (
	Blocks Protocol = "blocks" // colour half-blocks: works in every terminal, tmux and over SSH
	Kitty  Protocol = "kitty"  // kitty graphics protocol (kitty, Ghostty, ...)
	ITerm2 Protocol = "iterm2" // iTerm2 inline images (iTerm2, WezTerm, mintty, ...)
	Sixel  Protocol = "sixel"  // DEC sixel (foot, Konsole, mlterm, xterm -ti vt340, ...)
	None   Protocol = "none"   // no image previews, only the details
)

// IsGraphic reports whether the protocol draws pixels on top of the text grid
// (as opposed to producing ordinary text).
func (p Protocol) IsGraphic() bool { return p == Kitty || p == ITerm2 || p == Sixel }

// Detect picks the best protocol from the environment. SUCHI_IMAGE
// (kitty, iterm2, sixel, blocks, none) overrides the guess. Multiplexers
// get blocks because they do not pass graphics through by default.
func Detect(getenv func(string) string) Protocol {
	switch v := strings.ToLower(getenv("SUCHI_IMAGE")); v {
	case "kitty", "iterm2", "sixel", "blocks", "none":
		return Protocol(v)
	}
	term, prog := getenv("TERM"), getenv("TERM_PROGRAM")
	switch {
	case getenv("TMUX") != "" || getenv("STY") != "" || strings.HasPrefix(term, "screen") || strings.HasPrefix(term, "tmux"):
		return Blocks
	case term == "linux" || term == "dumb":
		return Blocks
	case getenv("KITTY_WINDOW_ID") != "" || term == "xterm-kitty" || prog == "ghostty" || getenv("GHOSTTY_RESOURCES_DIR") != "":
		return Kitty
	case prog == "iTerm.app" || prog == "WezTerm" || prog == "mintty" || getenv("LC_TERMINAL") == "iTerm2":
		return ITerm2
	case strings.HasPrefix(term, "foot") || strings.Contains(term, "sixel") || strings.HasPrefix(term, "mlterm") ||
		strings.HasPrefix(term, "contour") || getenv("KONSOLE_VERSION") != "":
		return Sixel
	}
	return Blocks
}

// CellSize returns the size of one terminal cell in pixels, or a typical
// 8x16 when the terminal does not report it.
func CellSize() (w, h int) {
	for _, fd := range []uintptr{1, 0, 2} {
		var ws struct{ Row, Col, X, Y uint16 }
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e == 0 &&
			ws.X > 0 && ws.Y > 0 && ws.Col > 0 && ws.Row > 0 {
			return int(ws.X) / int(ws.Col), int(ws.Y) / int(ws.Row)
		}
	}
	return 8, 16
}

// --- blocks ("Unicode mosaic" / ANSI art) -----------------------------------

const ramp = " .:-=+*#%@"

// RenderBlocks draws img into a cols x rows cell area using the upper
// half-block "▀" with the top pixel as foreground and the bottom pixel as
// background, so every cell shows two pixels in true colour (Lip Gloss
// reduces the colours for 256-, 16- and no-colour terminals; with no colour
// at all a brightness ramp is used). The cell grid is an ntcharts canvas.
func RenderBlocks(img image.Image, cols, rows int) string {
	cols, rows = max(cols, 1), max(rows, 1)
	pix := Resize(img, cols, rows*2)
	if lipgloss.ColorProfile() == termenv.Ascii {
		var b strings.Builder
		for y := 0; y < rows; y++ {
			if y > 0 {
				b.WriteByte('\n')
			}
			for x := 0; x < cols; x++ {
				t, bo := pix.NRGBAAt(x, 2*y), pix.NRGBAAt(x, 2*y+1)
				lum := (luma(t) + luma(bo)) / 2
				b.WriteByte(ramp[min(len(ramp)-1, int(lum*float64(len(ramp))))])
			}
		}
		return b.String()
	}
	c := canvas.New(cols, rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			top, bottom := pix.NRGBAAt(x, 2*y), pix.NRGBAAt(x, 2*y+1)
			style := lipgloss.NewStyle().
				Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", top.R, top.G, top.B))).
				Background(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", bottom.R, bottom.G, bottom.B)))
			c.SetCell(canvas.Point{X: x, Y: y}, canvas.NewCellWithStyle('▀', style))
		}
	}
	return c.View()
}

func luma(c interface{ RGBA() (r, g, b, a uint32) }) float64 {
	r, g, b, _ := c.RGBA()
	return (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 65535
}

// --- graphics protocols ------------------------------------------------------

// Graphic renders img as the escape sequence for a graphics protocol, scaled
// to cols x rows cells of cellW x cellH pixels. The caller writes it to the
// terminal at the cursor position of the area.
func Graphic(p Protocol, img image.Image, cols, rows, cellW, cellH int) (string, error) {
	pw, ph := max(cols*cellW, 1), max(rows*cellH, 1)
	scaled := Resize(img, pw, ph)
	switch p {
	case Sixel:
		var b bytes.Buffer
		enc := sixel.NewEncoder(&b)
		enc.Dither = true
		enc.Colors = 128
		if err := enc.Encode(scaled); err != nil {
			return "", fmt.Errorf("sixel encode: %w", err)
		}
		return b.String(), nil
	case Kitty, ITerm2:
		data, err := EncodePNG(scaled)
		if err != nil {
			return "", err
		}
		if p == Kitty {
			return KittySequence(data, cols, rows), nil
		}
		return ITerm2Sequence(data, cols, rows), nil
	}
	return "", fmt.Errorf("%q is not a graphics protocol", p)
}

// KittySequence transmits and displays a PNG in a cols x rows cell area using
// the kitty graphics protocol (chunked base64, cursor left in place).
func KittySequence(pngData []byte, cols, rows int) string {
	enc := base64.StdEncoding.EncodeToString(pngData)
	const chunk = 4096
	var b strings.Builder
	for i := 0; i < len(enc); i += chunk {
		end := min(i+chunk, len(enc))
		more := 1
		if end == len(enc) {
			more = 0
		}
		if i == 0 {
			fmt.Fprintf(&b, "\x1b_Ga=T,f=100,t=d,c=%d,r=%d,C=1,q=2,m=%d;", cols, rows, more)
		} else {
			fmt.Fprintf(&b, "\x1b_Gq=2,m=%d;", more)
		}
		b.WriteString(enc[i:end])
		b.WriteString("\x1b\\")
	}
	return b.String()
}

// KittyDelete removes every image placed with the kitty protocol.
func KittyDelete() string { return "\x1b_Ga=d,d=A,q=2\x1b\\" }

// ITerm2Sequence displays a PNG inline in a cols x rows cell area.
func ITerm2Sequence(pngData []byte, cols, rows int) string {
	return fmt.Sprintf("\x1b]1337;File=inline=1;size=%d;width=%d;height=%d;preserveAspectRatio=1:%s\a",
		len(pngData), cols, rows, base64.StdEncoding.EncodeToString(pngData))
}

// Place wraps an image sequence so it is drawn with its top-left corner at
// the 1-based screen position (row, col) and the cursor is restored after.
func Place(seq string, row, col int) string {
	return fmt.Sprintf("\x1b7\x1b[%d;%dH%s\x1b8", row, col, seq)
}
