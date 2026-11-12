// Package tui is suchi's terminal interface: a Clip tab with the clipboard
// history (text, images, copied files) and a Snip tab with snippets (a text
// expander), switched with Tab or the mouse.
package tui

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/dip-bash/suchi/internal/preview"
	"github.com/dip-bash/suchi/internal/store"
	"github.com/dip-bash/suchi/internal/textutil"
)

type tabID int

const (
	tabClips tabID = iota
	tabSnips
)

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayView             // read-only details of a clipboard entry
	overlayForm             // add / edit a snippet
	overlayHelp
)

// Result is what the user chose; Chosen is false when they quit without choosing.
type Result struct {
	Chosen bool
	Kind   string // store.ClipText, store.ClipImage or store.ClipFile
	Text   string // the text, the image's name, or the file paths (one per line)
	MIME   string
	Data   []byte // image bytes
}

type clipItem struct {
	c     store.Clip
	disp  []rune // searchable single-line text
	low   []rune
	paths []string // ClipFile: the copied paths
	label string   // type badge for images and files ("PNG", "MP4"...)
	cat   preview.Category
}

type snipItem struct {
	s       store.Snippet
	name    []rune
	nameLow []rune
	preview []rune
}

type row struct {
	idx     int
	matches []int
}

type pane struct {
	query    input
	sel, top int
}

type tickMsg time.Time

type execDoneMsg struct {
	name string
	err  error
}

var zonesOnce sync.Once

type model struct {
	ctx    context.Context
	st     *store.Store
	styles styles
	g      glyphs

	w, h   int
	tab    tabID
	search bool
	fuzzy  bool
	panes  [2]pane
	clips  []clipItem
	snips  []snipItem
	rows   [2][]row

	dataVer int64

	overlay overlayKind
	pop     popup
	fm      form

	flash      string
	flashUntil time.Time
	armed      int64 // snippet id waiting for a second "d" to be deleted

	pill, scroll, open spring
	frameRunning       bool

	lastClickID string
	lastClickAt time.Time

	proto        preview.Protocol
	cellW, cellH int
	out          io.Writer // where graphics escape sequences go (the terminal)

	result Result
}

func newModel(ctx context.Context, st *store.Store, opts Options) *model {
	zonesOnce.Do(zone.NewGlobal)
	g := unicodeGlyphs
	if useASCII(opts.ASCII) {
		g = asciiGlyphs
	}
	m := &model{ctx: ctx, st: st, styles: newStyles(), g: g, w: 80, h: 24,
		proto: opts.Protocol, cellW: 8, cellH: 16, out: opts.Out}
	m.pill, m.scroll, m.open = newAnimations()
	m.dataVer, _ = st.DataVersion(ctx)
	m.reload()
	return m
}

func (m *model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) setFlash(s string) {
	m.flash = s
	m.flashUntil = time.Now().Add(4 * time.Second)
}

// selectedID returns the id of the highlighted entry of a tab (0 if none).
func (m *model) selectedID(t tabID) int64 {
	p := m.panes[t]
	if p.sel < 0 || p.sel >= len(m.rows[t]) {
		return 0
	}
	i := m.rows[t][p.sel].idx
	if t == tabClips {
		return m.clips[i].c.ID
	}
	return m.snips[i].s.ID
}

func (m *model) selectedClip() (clipItem, bool) {
	if p := m.panes[tabClips]; p.sel >= 0 && p.sel < len(m.rows[tabClips]) {
		return m.clips[m.rows[tabClips][p.sel].idx], true
	}
	return clipItem{}, false
}

func (m *model) selectedSnip() (snipItem, bool) {
	if p := m.panes[tabSnips]; p.sel >= 0 && p.sel < len(m.rows[tabSnips]) {
		return m.snips[m.rows[tabSnips][p.sel].idx], true
	}
	return snipItem{}, false
}

// reload re-reads both lists, keeping each tab's highlighted entry.
func (m *model) reload() {
	keep := [2]int64{m.selectedID(tabClips), m.selectedID(tabSnips)}

	clips, err := m.st.ListClips(m.ctx)
	if err != nil {
		m.setFlash("could not read history: " + err.Error())
	}
	m.clips = m.clips[:0]
	for _, c := range clips {
		d := []rune(textutil.Display(c.Preview))
		it := clipItem{c: c, disp: d, low: textutil.Lower(d)}
		switch c.Kind {
		case store.ClipImage:
			it.label, it.cat = preview.LabelForMime(c.Mime), preview.CatImage
		case store.ClipFile:
			it.paths = strings.Split(c.Preview, "\n")
			it.label, it.cat = preview.Classify(it.paths[0], false)
		}
		m.clips = append(m.clips, it)
	}
	snips, err := m.st.ListSnippets(m.ctx)
	if err != nil {
		m.setFlash("could not read snippets: " + err.Error())
	}
	m.snips = m.snips[:0]
	for _, s := range snips {
		n := []rune(textutil.Display(s.Name))
		m.snips = append(m.snips, snipItem{
			s: s, name: n, nameLow: textutil.Lower(n),
			preview: []rune(textutil.Display(s.Body)),
		})
	}
	for _, t := range []tabID{tabClips, tabSnips} {
		m.refilter(t)
		m.selectID(t, keep[t])
	}
}

// refilter recomputes the visible rows of a tab from its query.
func (m *model) refilter(t tabID) {
	q := textutil.Lower([]rune(m.panes[t].query.String()))
	rows := m.rows[t][:0]
	if t == tabClips {
		for i, it := range m.clips {
			if idx, ok := textutil.Match(q, it.low, m.fuzzy); ok {
				rows = append(rows, row{i, idx})
			}
		}
	} else {
		for i, it := range m.snips { // snippets are searched by name
			if idx, ok := textutil.Match(q, it.nameLow, m.fuzzy); ok {
				rows = append(rows, row{i, idx})
			}
		}
	}
	m.rows[t] = rows
	m.clamp(t)
}

// selectID moves the highlight to the entry with the given id, if visible.
func (m *model) selectID(t tabID, id int64) {
	if id == 0 {
		m.clamp(t)
		return
	}
	for pos, r := range m.rows[t] {
		var rid int64
		if t == tabClips {
			rid = m.clips[r.idx].c.ID
		} else {
			rid = m.snips[r.idx].s.ID
		}
		if rid == id {
			m.panes[t].sel = pos
			break
		}
	}
	m.clamp(t)
}

// listHeight is the number of list rows: the screen minus header, rule and footer.
func (m *model) listHeight() int {
	if h := m.h - 3; h > 1 {
		return h
	}
	return 1
}

// clamp keeps the highlight inside the list and the list scrolled to show it.
func (m *model) clamp(t tabID) {
	p := &m.panes[t]
	n := len(m.rows[t])
	if p.sel >= n {
		p.sel = n - 1
	}
	if p.sel < 0 {
		p.sel = 0
	}
	h := m.listHeight()
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+h {
		p.top = p.sel - h + 1
	}
	if p.top < 0 {
		p.top = 0
	}
}

func (m *model) move(delta int) {
	t := m.tab
	m.panes[t].sel += delta
	m.clamp(t)
}

// setTab switches tab: the pill slides, the list does not animate.
func (m *model) setTab(t tabID) {
	if t == m.tab {
		return
	}
	m.tab = t
	m.armed = 0
	m.clamp(t)
	m.pill.set(float64(tabStart(t)))
	m.scroll.jump(float64(m.panes[t].top))
}

// syncAnim points the scroll spring at the list's current top row.
func (m *model) syncAnim() {
	m.scroll.set(float64(m.panes[m.tab].top))
}

func (m *model) animating() bool {
	return !m.pill.settled() || !m.scroll.settled() || !m.open.settled()
}

// kick starts the 60 fps frame loop if something is animating and it is not already running.
func (m *model) kick() tea.Cmd {
	m.syncAnim()
	if m.animating() && !m.frameRunning {
		m.frameRunning = true
		return frame()
	}
	return nil
}

// settle finishes every animation at once (used by tests and when a view must be exact).
func (m *model) settle() {
	for i := 0; i < 600 && m.animating(); i++ {
		m.pill.step()
		m.scroll.step()
		m.open.step()
	}
}

// tabStart is the column where a tab's pill begins inside the tab strip.
func tabStart(t tabID) int {
	if t == tabSnips {
		return 7
	}
	return 0
}
