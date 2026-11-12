package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/dip-bash/suchi/internal/preview"
	"github.com/dip-bash/suchi/internal/store"
	"github.com/dip-bash/suchi/internal/textutil"
)

// View implements tea.Model. zone.Scan strips the mouse-zone markers and
// records where each zone ended up on screen.
func (m *model) View() string {
	if m.w < 20 || m.h < 6 {
		return "terminal too small"
	}
	switch m.overlay {
	case overlayView:
		return zone.Scan(m.viewPopup())
	case overlayForm:
		return zone.Scan(m.viewForm())
	case overlayHelp:
		return zone.Scan(m.viewHelp())
	}
	lines := []string{m.viewHeader(), m.styles.dim.Render(strings.Repeat(m.g.rule, m.w-1))}
	lines = append(lines, m.viewList()...)
	lines = append(lines, m.viewFooter())
	return zone.Scan(strings.Join(lines, "\n"))
}

// tabCells is the tab strip: two 6-cell labels around a separator. The
// accent "pill" slides over the label of the active tab.
func (m *model) tabCells() []rune { return []rune(" Clip " + m.g.sep + " Snip ") }

func (m *model) viewHeader() string {
	st := m.styles
	cells := m.tabCells()
	start := int(math.Round(m.pill.pos))
	run := func(from, to int) string {
		var b strings.Builder
		for i := from; i < to; {
			inPill := i >= start && i < start+6
			j := i
			for j < to && (j >= start && j < start+6) == inPill {
				j++
			}
			sty := st.tabOff
			if inPill {
				sty = st.pill
			}
			b.WriteString(sty.Render(string(cells[i:j])))
			i = j
		}
		return b.String()
	}
	tabs := zone.Mark(zTabClip, run(0, 6)) + run(6, 7) + zone.Mark(zTabSnip, run(7, 13))

	p := &m.panes[m.tab]
	count := fmt.Sprintf("%d", len(m.rows[m.tab]))
	label := "Search "
	wide := m.w >= 44
	used := 1 + 13 // leading space + tab strip
	if wide {
		used += 1 + 1 + len(label) // separator, space, label
	}
	right := 0
	if m.w >= 30 {
		right = len(count) + 1
	}
	fw := m.w - 1 - used - right - 1
	if fw < 2 {
		fw = 2
	}
	field := p.query.render(fw, m.search, st, m.g.nl)
	if pad := fw - lipgloss.Width(field); pad > 0 {
		field += st.field.Render(strings.Repeat("_", pad))
	}
	lab := st.dim.Render(label)
	if m.search {
		lab = st.label.Render(label)
	}
	var b strings.Builder
	b.WriteString(" " + tabs)
	if wide {
		b.WriteString(" " + st.sep.Render(m.g.sep) + " ")
		b.WriteString(zone.Mark(zSearch, lab+field))
	} else {
		b.WriteString(" " + zone.Mark(zSearch, field))
	}
	if right > 0 {
		b.WriteString(" " + st.dim.Render(count))
	}
	return b.String()
}

// shownTop is the first visible row: the list glides toward its real
// scroll position, but the highlighted row is never scrolled out of sight.
func (m *model) shownTop(t tabID) int {
	p := m.panes[t]
	if t != m.tab {
		return p.top
	}
	h, n := m.listHeight(), len(m.rows[t])
	top := int(math.Round(m.scroll.pos))
	top = max(min(top, p.sel), p.sel-h+1)
	return max(0, min(top, max(0, n-h)))
}

func (m *model) viewList() []string {
	t := m.tab
	rows := m.rows[t]
	h := m.listHeight()
	out := make([]string, 0, h)
	if len(rows) == 0 {
		out = append(out, "")
		switch {
		case m.panes[t].query.String() != "":
			out = append(out, " "+m.styles.dim.Render("no matches"))
		case t == tabClips:
			out = append(out, " "+m.styles.dim.Render("history is empty - start the watcher with `suchi daemon`"))
		default:
			out = append(out, " "+m.styles.dim.Render("no snippets yet - press a to add one"))
		}
	}
	top := m.shownTop(t)
	nameW := 0
	if t == tabSnips {
		nameW = m.nameColumn(rows[min(top, len(rows)):min(top+h, len(rows))])
	}
	for i := top; i < len(rows) && len(out) < h; i++ {
		sel := i == m.panes[t].sel
		var ln string
		if t == tabClips {
			ln = m.clipRow(m.clips[rows[i].idx], rows[i].matches, sel)
		} else {
			ln = m.snipRow(m.snips[rows[i].idx], rows[i].matches, sel, nameW)
		}
		out = append(out, zone.Mark(zoneRow(i), ln))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}

// nameColumn is the width of the snippet-name column for the visible rows.
func (m *model) nameColumn(vis []row) int {
	w := 6
	for _, r := range vis {
		if n := textutil.Width(string(m.snips[r.idx].name)); n > w {
			w = n
		}
	}
	if limit := min(24, m.w/3); w > limit {
		w = limit
	}
	return w
}

// line assembles styled pieces of a row.
type line struct {
	b    strings.Builder
	used int
}

func (l *line) add(s string, st lipgloss.Style) {
	if s == "" {
		return
	}
	l.b.WriteString(st.Render(s))
	l.used += textutil.Width(s)
}

func (m *model) segments(l *line, rs []rune, matches []int, cols int, base, hit lipgloss.Style) {
	for _, s := range textutil.Layout(rs, matches, cols, m.g.ellipsis) {
		if s.Match {
			l.add(s.Text, hit)
		} else {
			l.add(s.Text, base)
		}
	}
}

// rowStyles returns the styles of a row, with the selection background applied.
func (m *model) rowStyles(selected bool) (base, hit, dim lipgloss.Style) {
	st := m.styles
	if selected {
		return st.sel, st.selMatch, st.dim.Background(cSelBg)
	}
	return st.text, st.match, st.dim
}

// rowPrefix writes the arrow, the pin and the spacing: " ➜ ● ".
func (m *model) rowPrefix(l *line, selected, pinned bool, base lipgloss.Style) {
	st := m.styles
	if selected {
		l.add(" ", base)
		l.add(m.g.arrow, st.arrow)
		l.add(" ", base)
	} else {
		l.add("   ", base)
	}
	if pinned {
		pin := st.pin
		if selected {
			pin = pin.Background(cSelBg)
		}
		l.add(m.g.pin, pin)
	} else {
		l.add(" ", base)
	}
	l.add(" ", base)
}

func (m *model) clipRow(it clipItem, matches []int, selected bool) string {
	st := m.styles
	base, hit, dim := m.rowStyles(selected)
	var l line
	m.rowPrefix(&l, selected, it.c.Pinned, base)

	// right side: the type badge for images and files, then the age
	right, rw := "", 0
	if it.c.Kind != store.ClipText {
		right = st.badge(it.label, it.cat)
		rw = textutil.Width(it.label) + 2
	}
	rel := textutil.Relative(it.c.LastActive(), time.Now())
	if m.w >= 40 && rel != "" {
		if right != "" {
			right += dim.Render(" ")
			rw++
		}
		right += dim.Render(rel)
		rw += len(rel)
	}
	if right != "" {
		rw++ // a space before it
	}

	ml := ""
	if it.c.Kind == store.ClipText && it.c.Multiline {
		ml = m.g.ml
	}
	cols := m.w - 1 - l.used - rw - textutil.Width(ml) - 1
	text, hits := it.disp, matches
	if it.c.Kind == store.ClipFile {
		text, hits = m.fileText(it, cols)
	}
	m.segments(&l, text, hits, cols, base, hit)
	l.add(ml, dim)
	if right != "" {
		l.add(strings.Repeat(" ", max(1, m.w-1-l.used-(rw-1))), base)
		l.b.WriteString(right)
	} else if selected {
		l.add(strings.Repeat(" ", max(0, m.w-1-l.used)), base)
	}
	return l.b.String()
}

// fileText is the one-line form of a copied-files entry: the (shortened)
// path of the first file and how many more there are.
func (m *model) fileText(it clipItem, cols int) ([]rune, []int) {
	more := ""
	if n := len(it.paths); n > 1 {
		more = fmt.Sprintf(" +%d", n-1)
	}
	shown := []rune(preview.ShortPath(textutil.Display(it.paths[0]), max(4, cols-len(more))) + more)
	q := textutil.Lower([]rune(m.panes[tabClips].query.String()))
	idx, _ := textutil.Match(q, textutil.Lower(shown), m.fuzzy)
	return shown, idx
}

func (m *model) snipRow(it snipItem, matches []int, selected bool, nameW int) string {
	st := m.styles
	base, hit, dim := m.rowStyles(selected)
	bold := st.bold
	cmd := st.cmd
	if selected {
		bold, cmd = bold.Background(cSelBg), cmd.Background(cSelBg)
	}
	var l line
	if selected {
		l.add(" ", base)
		l.add(m.g.arrow, st.arrow)
		l.add(" ", base)
	} else {
		l.add("   ", base)
	}
	if it.s.Kind == store.KindCommand {
		l.add(m.g.cmd, cmd)
	} else {
		l.add(" ", base)
	}
	l.add(" ", base)

	// name column (matches are highlighted), padded to nameW
	before := l.used
	m.segments(&l, it.name, matches, nameW, bold, hit)
	if pad := nameW - (l.used - before); pad > 0 {
		l.add(strings.Repeat(" ", pad), base)
	}
	l.add("  ", base)

	ml := ""
	if strings.Contains(it.s.Body, "\n") {
		ml = m.g.ml
	}
	cols := m.w - 1 - l.used - textutil.Width(ml) - 1
	m.segments(&l, it.preview, nil, cols, dim, dim)
	l.add(ml, dim)
	if selected {
		l.add(strings.Repeat(" ", max(0, m.w-1-l.used)), base)
	}
	return l.b.String()
}

type hint struct{ k, v string }

func (m *model) footerHints() []hint {
	switch {
	case m.search:
		return []hint{{"Enter", "copy"}, {"↑↓", "move"}, {"Esc", "done"}}
	}
	var hs []hint
	if m.tab == tabClips {
		hs = []hint{{"Enter", "copy"}, {"e", "view"}, {"d", "delete"}}
	} else {
		hs = []hint{{"Enter", "copy"}, {"r", "run"}, {"a", "add"}, {"e", "edit"}, {"d", "delete"}}
	}
	if len(m.undo) > 0 {
		hs = append(hs, hint{"u", "undo"})
	}
	if m.tab == tabClips {
		return append(hs, hint{"p", "pin"}, hint{"i", "search"}, hint{"Tab", "snip"}, hint{"?", "help"})
	}
	return append(hs, hint{"i", "search"}, hint{"Tab", "clip"}, hint{"?", "help"})
}

func (m *model) viewFooter() string {
	st := m.styles
	if m.flash != "" {
		return " " + st.flash.Render(m.clip(m.flash, m.w-3))
	}
	hs := m.footerHints()
	var b strings.Builder
	b.WriteString(" ")
	used := 1
	for i, h := range hs {
		k := h.k
		if m.g.rule == "-" {
			k = strings.ReplaceAll(k, "↑↓", "up/down")
		}
		w := textutil.Width(k) + 1 + textutil.Width(h.v)
		if i > 0 {
			w += 2
		}
		if used+w > m.w-2 {
			break
		}
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(st.keycap.Render(k) + " " + st.dim.Render(h.v))
		used += w
	}
	return b.String()
}

// clip cuts plain text to cols columns, ending in an ellipsis.
func (m *model) clip(s string, cols int) string {
	var b strings.Builder
	for _, seg := range textutil.Layout([]rune(s), nil, cols, m.g.ellipsis) {
		b.WriteString(seg.Text)
	}
	return b.String()
}

func (m *model) viewHelp() string {
	type kv struct{ k, v string }
	help := []kv{
		{"Tab", "switch Clip / Snip"},
		{"j k ↑ ↓", "move"},
		{"i", "search (Esc leaves it)"},
		{"f", "toggle fuzzy search"},
		{"Enter", "copy and close"},
		{"d", "delete (press twice)"},
		{"u", "undo a delete"},
		{"", ""},
		{"Clip", ""},
		{"e", "view: text, picture, files"},
		{"p", "pin / unpin"},
		{"", ""},
		{"Snip", ""},
		{"a", "add a snippet"},
		{"e", "edit"},
		{"r", "run a command snippet"},
		{"", ""},
		{"Mouse", ""},
		{"click", "copy (command: run)"},
		{"right click", "same as e"},
		{"tabs", "click to switch"},
		{"search", "double click to type"},
		{"", ""},
		{"Esc", "close a popup / quit"},
	}
	if m.g.rule == "-" {
		help[1].k = "j k"
	}
	rows := []string{m.styles.bold.Render("suchi")}
	for _, h := range help {
		switch {
		case h.k == "" && h.v == "":
			rows = append(rows, "")
		case h.v == "":
			rows = append(rows, m.styles.label.Render(h.k))
		default:
			rows = append(rows, m.styles.keycap.Render(fmt.Sprintf("%-11s", h.k))+" "+m.clip(h.v, max(4, m.w-20)))
		}
	}
	rows = append(rows, "", m.styles.dim.Render("press any key"))
	for len(rows) > m.h-3 && len(rows) > 3 { // tiny terminals: drop the blank spacers first
		dropped := false
		for i, r := range rows {
			if r == "" {
				rows = append(rows[:i], rows[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			rows = rows[:m.h-3]
			break
		}
	}
	box := m.styles.box.MaxWidth(m.w).MaxHeight(m.h).Render(strings.Join(rows, "\n"))
	return m.centerAt(box, max(0, (m.w-lipgloss.Width(box))/2), max(0, (m.h-lipgloss.Height(box))/2))
}

func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
