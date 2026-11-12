package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/dip-bash/suchi/internal/preview"
)

// ac builds a colour for light and dark terminal backgrounds. Lip Gloss
// reduces hex colours automatically for 256-, 16- and no-colour terminals.
func ac(light, dark string) lipgloss.AdaptiveColor {
	return lipgloss.AdaptiveColor{Light: light, Dark: dark}
}

// The palette.
var (
	cAccent  = ac("#0e7490", "#7dcfff")
	cPurple  = ac("#6d28d9", "#bb9af7")
	cGreen   = ac("#15803d", "#9ece6a")
	cYellow  = ac("#a16207", "#e0af68")
	cRed     = ac("#b91c1c", "#f7768e")
	cBlue    = ac("#1d4ed8", "#7aa2f7")
	cMuted   = ac("#6b7280", "#7a84ad")
	cText    = ac("#1f2937", "#c0caf5")
	cSelBg   = ac("#dbeafe", "#283457")
	cOnColor = ac("#ffffff", "#16161e") // text drawn on a coloured background
)

type styles struct {
	pill, tabOff, sep   lipgloss.Style
	dim, bold, text     lipgloss.Style
	sel, selMatch       lipgloss.Style
	arrow, match        lipgloss.Style
	pin, cmd            lipgloss.Style
	flash, err, label   lipgloss.Style
	cursor, box         lipgloss.Style
	keycap, btn, btnAlt lipgloss.Style
	field               lipgloss.Style
}

func newStyles() styles {
	return styles{
		pill:     lipgloss.NewStyle().Bold(true).Foreground(cOnColor).Background(cAccent),
		tabOff:   lipgloss.NewStyle().Foreground(cMuted),
		sep:      lipgloss.NewStyle().Foreground(cMuted),
		dim:      lipgloss.NewStyle().Foreground(cMuted),
		bold:     lipgloss.NewStyle().Bold(true).Foreground(cText),
		text:     lipgloss.NewStyle().Foreground(cText),
		sel:      lipgloss.NewStyle().Foreground(cText).Background(cSelBg),
		selMatch: lipgloss.NewStyle().Foreground(cYellow).Background(cSelBg).Bold(true),
		arrow:    lipgloss.NewStyle().Foreground(cAccent).Background(cSelBg).Bold(true),
		match:    lipgloss.NewStyle().Foreground(cYellow).Bold(true),
		pin:      lipgloss.NewStyle().Foreground(cYellow),
		cmd:      lipgloss.NewStyle().Foreground(cPurple).Bold(true),
		flash:    lipgloss.NewStyle().Foreground(cYellow),
		err:      lipgloss.NewStyle().Foreground(cRed),
		label:    lipgloss.NewStyle().Foreground(cAccent).Bold(true),
		cursor:   lipgloss.NewStyle().Reverse(true),
		box:      lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(cAccent).Padding(0, 1),
		keycap:   lipgloss.NewStyle().Foreground(cAccent).Bold(true),
		btn:      lipgloss.NewStyle().Foreground(cOnColor).Background(cAccent).Bold(true).Padding(0, 1),
		btnAlt:   lipgloss.NewStyle().Foreground(cText).Background(cSelBg).Padding(0, 1),
		field:    lipgloss.NewStyle().Foreground(cMuted),
	}
}

// badge renders a small coloured type tag such as " PNG " or " MP4 ".
func (s styles) badge(label string, cat preview.Category) string {
	var c lipgloss.AdaptiveColor
	switch cat {
	case preview.CatImage:
		c = cPurple
	case preview.CatVideo:
		c = cBlue
	case preview.CatAudio:
		c = cGreen
	case preview.CatDoc:
		c = cYellow
	case preview.CatArchive:
		c = cRed
	case preview.CatCode:
		c = cAccent
	case preview.CatFolder:
		c = cYellow
	default:
		c = cMuted
	}
	return lipgloss.NewStyle().Bold(true).Foreground(cOnColor).Background(c).Padding(0, 1).Render(label)
}

// glyphs are the few non-ASCII symbols; the Linux console gets plain ASCII.
type glyphs struct {
	arrow, pin, cmd, nl, ml, ellipsis, rule, sep, radioOn, radioOff, dot string
}

var (
	unicodeGlyphs = glyphs{arrow: "➜", pin: "●", cmd: "$", nl: "↵", ml: " ↵", ellipsis: "…", rule: "─", sep: "│", radioOn: "(•)", radioOff: "( )", dot: "·"}
	asciiGlyphs   = glyphs{arrow: ">", pin: "*", cmd: "$", nl: "~", ml: " ~", ellipsis: "...", rule: "-", sep: "|", radioOn: "(*)", radioOff: "( )", dot: "|"}
)

// useASCII reports whether to avoid non-ASCII glyphs: forced by the caller,
// on the Linux console, or when the locale is not UTF-8.
func useASCII(force bool) bool {
	if force || os.Getenv("TERM") == "linux" {
		return true
	}
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToLower(os.Getenv(k)); v != "" {
			return !strings.Contains(v, "utf-8") && !strings.Contains(v, "utf8")
		}
	}
	return false
}
