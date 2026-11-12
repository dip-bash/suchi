// Package textutil holds the terminal-text helpers shared by the TUI and the
// clipboard code: sanitising clipboard text for display, width-aware
// truncation and wrapping, and the search matchers.
package textutil

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/mattn/go-runewidth"
)

// DisplayCap is how many characters of an entry are indexed for display and search.
const DisplayCap = 8192

var (
	wsRe   = regexp.MustCompile(`\s+`)
	ctrlRe = regexp.MustCompile("[\x00-\x1f\x7f-\u009f\u2028\u2029]")
	// zero-width / bidi / format characters (unicode category Cf)
	invisRe = regexp.MustCompile("[\u00ad\u061c\u180e\u200b-\u200f\u202a-\u202e\u2060-\u2064\u2066-\u206f\ufeff\ufff9-\ufffb\U000e0001\U000e0020-\U000e007f]")
)

// Display returns a single terminal-safe line for an entry. Control
// characters (including ESC) can never reach the terminal. It is used for
// both searching and drawing so match indices always line up.
func Display(s string) string {
	if r := []rune(s); len(r) > DisplayCap {
		s = string(r[:DisplayCap])
	}
	s = invisRe.ReplaceAllString(s, "")
	s = ctrlRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// Lower lower-cases rune by rune so the result has the same length as the
// input and match indices stay valid.
func Lower(rs []rune) []rune {
	out := make([]rune, len(rs))
	for i, r := range rs {
		out[i] = unicode.ToLower(r)
	}
	return out
}

// Match returns the rune indices of query inside low (both already lower-cased).
// With fuzzy set the query characters only have to appear in order.
func Match(query, low []rune, fuzzy bool) ([]int, bool) {
	if len(query) == 0 {
		return nil, true
	}
	if fuzzy {
		idx := make([]int, 0, len(query))
		pos := 0
		for _, qc := range query {
			if qc == ' ' {
				continue
			}
			found := -1
			for i := pos; i < len(low); i++ {
				if low[i] == qc {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, false
			}
			idx = append(idx, found)
			pos = found + 1
		}
		return idx, true
	}
	for i := 0; i+len(query) <= len(low); i++ {
		ok := true
		for j, qc := range query {
			if low[i+j] != qc {
				ok = false
				break
			}
		}
		if ok {
			idx := make([]int, len(query))
			for j := range idx {
				idx[j] = i + j
			}
			return idx, true
		}
	}
	return nil, false
}

// Width is the number of terminal columns s occupies.
func Width(s string) int { return runewidth.StringWidth(s) }

// Segment is a run of text that either is or is not part of a search match.
type Segment struct {
	Text  string
	Match bool
}

// Layout cuts rs to at most cols terminal columns, appending ellipsis when
// something was cut, and marks the runes listed in matches.
func Layout(rs []rune, matches []int, cols int, ellipsis string) []Segment {
	if cols <= 0 {
		return nil
	}
	hit := make(map[int]bool, len(matches))
	for _, m := range matches {
		hit[m] = true
	}
	total := 0
	for _, r := range rs {
		total += runewidth.RuneWidth(r)
	}
	limit := cols
	cut := total > cols
	if cut {
		limit = cols - Width(ellipsis)
		if limit < 0 {
			limit = 0
		}
	}
	var segs []Segment
	var b strings.Builder
	cur, used := false, 0
	flush := func() {
		if b.Len() > 0 {
			segs = append(segs, Segment{b.String(), cur})
			b.Reset()
		}
	}
	for i, r := range rs {
		w := runewidth.RuneWidth(r)
		if used+w > limit {
			break
		}
		if hit[i] != cur {
			flush()
			cur = hit[i]
		}
		b.WriteRune(r)
		used += w
	}
	flush()
	if cut {
		segs = append(segs, Segment{Text: ellipsis})
	}
	return segs
}

// Wrap hard-wraps text to width columns for the full-text popup. Newlines are
// kept, tabs become four spaces and other control characters become a middle
// dot so the terminal never interprets clipboard content.
func Wrap(text string, width int) []string {
	if width < 4 {
		width = 4
	}
	text = invisRe.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []string
	for _, para := range strings.Split(text, "\n") {
		para = strings.ReplaceAll(para, "\t", "    ")
		clean := make([]rune, 0, len(para))
		for _, r := range para {
			if unicode.IsControl(r) {
				r = '·'
			}
			clean = append(clean, r)
		}
		if len(clean) == 0 {
			out = append(out, "")
			continue
		}
		for len(clean) > 0 {
			w, n := 0, 0
			for n < len(clean) {
				rw := runewidth.RuneWidth(clean[n])
				if w+rw > width {
					break
				}
				w += rw
				n++
			}
			if n == 0 {
				n = 1
			}
			atSpace := false
			if n < len(clean) {
				if clean[n] == ' ' { // the line ends exactly at a word boundary
					atSpace = true
				} else { // otherwise prefer breaking at a space in the second half of the line
					for k := n; k > n/2; k-- {
						if clean[k-1] == ' ' {
							n, atSpace = k, true
							break
						}
					}
				}
			}
			out = append(out, strings.TrimRight(string(clean[:n]), " "))
			clean = clean[n:]
			if atSpace { // do not start the next line with the separating spaces
				for len(clean) > 0 && clean[0] == ' ' {
					clean = clean[1:]
				}
			}
		}
	}
	return out
}

// Relative formats a timestamp as "now", "5m", "3h" or "2d".
func Relative(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
