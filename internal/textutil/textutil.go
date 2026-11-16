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
	// Find the byte boundary of DisplayCap runes without converting the whole
	// string to []rune. Snippet bodies can be large, while the list only ever
	// displays this small prefix.
	if len(s) > DisplayCap {
		count := 0
		for i := range s {
			if count == DisplayCap {
				s = s[:i]
				break
			}
			count++
		}
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
			for pos < len(low) && low[pos] != qc {
				pos++
			}
			if pos == len(low) {
				return nil, false
			}
			idx = append(idx, pos)
			pos++
		}
		return idx, true
	}
	if len(query) > len(low) {
		return nil, false
	}

	// Most interactive queries are only a few runes long. The straightforward
	// scan avoids building a KMP prefix table for every history row; reserve
	// KMP for longer queries where its linear bound matters.
	if len(query) <= 8 {
		for i := 0; i+len(query) <= len(low); i++ {
			j := 0
			for j < len(query) && low[i+j] == query[j] {
				j++
			}
			if j == len(query) {
				idx := make([]int, len(query))
				for k := range idx {
					idx[k] = i + k
				}
				return idx, true
			}
		}
		return nil, false
	}

	// Knuth-Morris-Pratt avoids rescanning the same prefix when a query
	// contains repeated characters (e.g. "aaaaab") and is not present.
	prefix := make([]int, len(query))
	for i, j := 1, 0; i < len(query); i++ {
		for j > 0 && query[i] != query[j] {
			j = prefix[j-1]
		}
		if query[i] == query[j] {
			j++
		}
		prefix[i] = j
	}
	matched := 0
	for i, r := range low {
		for matched > 0 && r != query[matched] {
			matched = prefix[matched-1]
		}
		if r == query[matched] {
			matched++
		}
		if matched == len(query) {
			first := i - len(query) + 1
			idx := make([]int, len(query))
			for j := range idx {
				idx[j] = first + j
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

	// Most rows are much wider than the terminal, but only a screenful of
	// columns is visible. Stop measuring as soon as truncation is certain
	// instead of calculating the width of every rune on every redraw.
	visibleWidth := 0
	cut := false
	for _, r := range rs {
		visibleWidth += max(0, runewidth.RuneWidth(r))
		if visibleWidth > cols {
			cut = true
			break
		}
	}
	limit := cols
	if cut {
		limit = max(0, cols-Width(ellipsis))
	}

	var segs []Segment
	var b strings.Builder
	matchPos, used := 0, 0
	cur, haveRun := false, false
	flush := func() {
		if haveRun && b.Len() > 0 {
			segs = append(segs, Segment{b.String(), cur})
			b.Reset()
		}
		haveRun = false
	}
	for i, r := range rs {
		w := max(0, runewidth.RuneWidth(r))
		if used+w > limit {
			break
		}
		for matchPos < len(matches) && matches[matchPos] < i {
			matchPos++
		}
		isMatch := matchPos < len(matches) && matches[matchPos] == i
		if isMatch {
			matchPos++
		}
		if !haveRun {
			cur, haveRun = isMatch, true
		} else if isMatch != cur {
			flush()
			cur, haveRun = isMatch, true
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
