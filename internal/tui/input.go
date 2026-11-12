package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// input is a minimal single-line text field with a cursor (newlines allowed
// in "multiline" fields are drawn as a glyph).
type input struct {
	r   []rune
	cur int
}

func (in *input) set(s string)   { in.r = []rune(s); in.cur = len(in.r) }
func (in *input) String() string { return string(in.r) }

func (in *input) insert(rs []rune) {
	out := make([]rune, 0, len(in.r)+len(rs))
	out = append(out, in.r[:in.cur]...)
	out = append(out, rs...)
	out = append(out, in.r[in.cur:]...)
	in.r = out
	in.cur += len(rs)
}

func (in *input) backspace() {
	if in.cur > 0 {
		in.r = append(in.r[:in.cur-1], in.r[in.cur:]...)
		in.cur--
	}
}

func (in *input) del() {
	if in.cur < len(in.r) {
		in.r = append(in.r[:in.cur], in.r[in.cur+1:]...)
	}
}

// editKey applies a key press to the input and reports whether it was an
// editing key. Pasted text arrives as one KeyRunes message.
func editKey(in *input, msg tea.KeyMsg, multiline bool) bool {
	switch msg.String() {
	case "backspace":
		in.backspace()
	case "delete":
		in.del()
	case "left":
		if in.cur > 0 {
			in.cur--
		}
	case "right":
		if in.cur < len(in.r) {
			in.cur++
		}
	case "home":
		in.cur = 0
	case "end":
		in.cur = len(in.r)
	case "ctrl+j":
		if !multiline {
			return false
		}
		in.insert([]rune{'\n'})
	default:
		if (msg.Type != tea.KeyRunes && msg.Type != tea.KeySpace) || msg.Alt || len(msg.Runes) == 0 {
			return false
		}
		rs := make([]rune, 0, len(msg.Runes))
		for _, r := range msg.Runes {
			switch {
			case r == '\r':
				continue
			case r == '\n' && multiline:
				rs = append(rs, r)
			case r == '\n' || r == '\t':
				rs = append(rs, ' ')
			case unicode.IsControl(r):
				continue
			default:
				rs = append(rs, r)
			}
		}
		if len(rs) == 0 {
			return false
		}
		in.insert(rs)
	}
	return true
}

// render draws the input in width columns, scrolling so the cursor stays visible.
func (in *input) render(width int, focused bool, st styles, nl string) string {
	if width < 2 {
		return ""
	}
	disp := make([]rune, len(in.r))
	for i, r := range in.r {
		switch {
		case r == '\n':
			disp[i] = []rune(nl)[0]
		case unicode.IsControl(r):
			disp[i] = '·'
		default:
			disp[i] = r
		}
	}
	start := 0
	for start < in.cur && runewidth.StringWidth(string(disp[start:in.cur])) >= width {
		start++
	}
	clip := func(rs []rune, room int) string {
		var b strings.Builder
		used := 0
		for _, r := range rs {
			w := runewidth.RuneWidth(r)
			if used+w > room {
				break
			}
			b.WriteRune(r)
			used += w
		}
		return b.String()
	}
	if !focused {
		return clip(disp[start:], width)
	}
	before := string(disp[start:in.cur])
	at := " "
	rest := []rune{}
	if in.cur < len(disp) {
		at = string(disp[in.cur])
		rest = disp[in.cur+1:]
	}
	room := width - runewidth.StringWidth(before) - runewidth.StringWidth(at)
	if room < 0 {
		room = 0
	}
	return before + st.cursor.Render(at) + clip(rest, room)
}
