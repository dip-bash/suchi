package tui

import (
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/dip-bash/suchi/internal/preview"
	"github.com/dip-bash/suchi/internal/store"
	"github.com/dip-bash/suchi/internal/textutil"
)

// maxPreviewFile is the biggest image file read from disk for a preview.
const maxPreviewFile = 32 << 20

type drawImageMsg struct{ gen int }

// popup is the read-only view of a clipboard entry: text in a scrollable
// viewport, or a picture with its details underneath.
type popup struct {
	clip   clipItem
	text   string // full text of a text entry
	length int
	title  string
	label  string
	cat    preview.Category
	meta   string

	lines   []string // wrapped text, or styled detail lines
	inner   int      // width the lines were built for
	details func(inner int) []string
	vp      viewport.Model

	img          image.Image
	imgW, imgH   int
	imgNote      string // why there is no picture
	art          map[string]string
	drawn        bool
	gen          int
	imgTop, imgX int // 0-based screen position of the picture (graphics protocols)
}

// popLayout is the geometry of the popup box for the current terminal size.
type popLayout struct {
	boxW, boxH, x0, y0, inner int
	spacer                    int // blank row above the buttons, when there is room
	hasImg                    bool
	imgCols, imgRows, imgLeft int
	vpH                       int
}

func (m *model) newViewport() viewport.Model {
	vp := viewport.New(10, 1)
	vp.KeyMap = viewport.KeyMap{
		Up:       key.NewBinding(key.WithKeys("up", "k")),
		Down:     key.NewBinding(key.WithKeys("down", "j")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", " ")),
	}
	vp.MouseWheelEnabled = true
	return vp
}

// openView builds the popup for a clipboard entry.
func (m *model) openView(c clipItem) tea.Cmd {
	p := popup{clip: c, vp: m.newViewport(), art: map[string]string{}, label: "TEXT", cat: preview.CatText}
	p.gen = m.pop.gen + 1
	switch c.c.Kind {
	case store.ClipImage:
		m.buildImagePopup(&p, c)
	case store.ClipFile:
		m.buildFilesPopup(&p, c)
	default:
		text, err := m.st.ClipText(m.ctx, c.c.ID)
		if err != nil {
			m.setFlash(err.Error())
			return nil
		}
		p.text, p.length = text, len([]rune(text))
		p.title = "Clipboard text"
		p.meta = commas(p.length) + " characters"
	}
	m.pop = p
	m.overlay = overlayView
	if c.c.Kind == store.ClipText {
		m.open.jump(0) // unfold
		m.open.set(1)
		return nil
	}
	m.open.jump(1)
	return m.scheduleImage()
}

func (m *model) buildImagePopup(p *popup, c clipItem) {
	p.label, p.cat = c.label, preview.CatImage
	p.title = c.c.Preview
	data, mime, err := m.st.ClipData(m.ctx, c.c.ID)
	if err != nil {
		p.imgNote = err.Error()
		return
	}
	w, h, format, cfgErr := preview.Config(data)
	p.meta = preview.HumanSize(int64(len(data)))
	if cfgErr == nil {
		p.meta = fmt.Sprintf("%d × %d · %s", w, h, p.meta)
	}
	p.details = func(inner int) []string {
		return m.kv(inner, [][2]string{
			{"Name", c.c.Preview},
			{"Type", mime + " · " + strings.ToUpper(format)},
			{"Size", p.meta},
		})
	}
	m.loadPicture(p, data)
}

func (m *model) buildFilesPopup(p *popup, c clipItem) {
	paths := c.paths
	if full, err := m.st.ClipText(m.ctx, c.c.ID); err == nil {
		paths = strings.Split(full, "\n")
	}
	p.label, p.cat = c.label, c.cat
	infos := make([]preview.Info, len(paths))
	for i, path := range paths {
		infos[i] = preview.Stat(path)
	}
	p.title = "Copied file"
	if len(infos) > 1 {
		p.title = fmt.Sprintf("%d copied files", len(infos))
		p.label, p.cat = "FILES", preview.CatOther
	}
	p.meta = fmt.Sprintf("%d item", len(infos))
	if len(infos) != 1 {
		p.meta += "s"
	}
	p.details = func(inner int) []string {
		var out []string
		for i, in := range infos {
			if i > 0 {
				out = append(out, "")
			}
			head := m.styles.badge(in.Label, in.Category) + " " + m.styles.bold.Render(in.Name)
			switch {
			case !in.Exists:
				head += "  " + m.styles.err.Render("missing")
			case in.IsDir:
				head += "  " + m.styles.dim.Render("folder")
			default:
				head += "  " + m.styles.dim.Render(preview.HumanSize(in.Size)+" "+m.g.dot+" "+in.ModTime.Format("2006-01-02 15:04"))
			}
			out = append(out, head, "    "+m.styles.dim.Render(preview.ShortPath(in.Path, inner-4)))
		}
		return out
	}
	// Preview the first file when it is a picture we can draw.
	if in := infos[0]; in.Exists && !in.IsDir && preview.DecodableImage(in.Path) {
		if in.Size > maxPreviewFile {
			p.imgNote = "picture too large to preview"
		} else if data, err := os.ReadFile(in.Path); err == nil {
			m.loadPicture(p, data)
		}
	}
}

func (m *model) loadPicture(p *popup, data []byte) {
	if m.proto == preview.None {
		p.imgNote = "previews are turned off (SUCHI_IMAGE=none)"
		return
	}
	img, _, err := preview.Decode(data)
	if err != nil {
		p.imgNote = err.Error()
		return
	}
	p.img = img
	p.imgW, p.imgH = img.Bounds().Dx(), img.Bounds().Dy()
}

// kv formats label/value detail lines.
func (m *model) kv(inner int, pairs [][2]string) []string {
	out := make([]string, 0, len(pairs)+1)
	for _, kv := range pairs {
		val := m.clip(kv[1], max(1, inner-8))
		out = append(out, m.styles.label.Render(fmt.Sprintf("%-6s", kv[0]))+" "+m.styles.text.Render(val))
	}
	return out
}

// ensureLines (re)builds the wrapped text / detail lines for the current width.
func (m *model) ensureLines(inner int) {
	p := &m.pop
	if p.lines != nil && p.inner == inner {
		return
	}
	p.inner = inner
	switch {
	case p.details != nil:
		p.lines = p.details(inner)
	default:
		p.lines = textutil.Wrap(p.text, inner)
	}
	if p.imgNote != "" {
		p.lines = append(p.lines, "", m.styles.dim.Render(m.clip(p.imgNote, inner)))
	}
	p.vp.Width = inner
	p.vp.SetContent(strings.Join(p.lines, "\n"))
}

// cellPixels is the pixel size of one cell for fitting pictures.
func (m *model) cellPixels() (w, h int) {
	if m.proto.IsGraphic() {
		return m.cellW, m.cellH
	}
	return 1, 2 // half-blocks: one pixel wide, two tall per cell
}

// popLayout computes the popup geometry. During the unfold animation the
// text body is only partly shown.
func (m *model) popLayout() popLayout {
	var l popLayout
	l.boxW = max(10, min(m.w-2, 112))
	l.inner = l.boxW - 4
	if m.h >= 12 {
		l.spacer = 1
	}
	bodyMax := max(1, m.h-7-l.spacer) // border(2) + title + rule + buttons + spacer + one spare row
	m.ensureLines(l.inner)
	n := max(1, len(m.pop.lines))

	body := 0
	if m.pop.img != nil {
		d := min(n, 4)
		maxRows := bodyMax - d - 1
		if maxRows >= 3 && l.inner >= 8 {
			cw, ch := m.cellPixels()
			l.imgCols, l.imgRows = preview.Fit(m.pop.imgW, m.pop.imgH, l.inner, maxRows, cw, ch)
			l.hasImg = true
			l.imgLeft = (l.inner - l.imgCols) / 2
			body += l.imgRows + 1
		}
	}
	l.vpH = max(1, min(n, bodyMax-body))
	if m.overlay == overlayView && m.pop.clip.c.Kind == store.ClipText {
		l.vpH = max(1, int(math.Round(float64(l.vpH)*math.Min(1, math.Max(0, m.open.pos)))))
	}
	body += l.vpH
	l.boxH = body + 5 + l.spacer
	l.x0 = max(0, (m.w-l.boxW)/2)
	l.y0 = max(0, (m.h-l.boxH)/2)
	m.pop.imgTop = l.y0 + 3 // border + title + rule
	m.pop.imgX = l.x0 + 2 + l.imgLeft
	return l
}

func (m *model) blocksArt(cols, rows int) string {
	k := fmt.Sprintf("%dx%d", cols, rows)
	if a, ok := m.pop.art[k]; ok {
		return a
	}
	a := preview.RenderBlocks(m.pop.img, cols, rows)
	m.pop.art[k] = a
	return a
}

func (m *model) viewPopup() string {
	l := m.popLayout()
	st := m.styles
	p := &m.pop
	fit := lipgloss.NewStyle().MaxWidth(l.inner)

	// title row: type badge + title on the left, details on the right
	badge := st.badge(p.label, p.cat)
	right := st.dim.Render(m.clip(p.meta, l.inner/2))
	titleRoom := l.inner - lipgloss.Width(badge) - 1 - lipgloss.Width(right) - 2
	title := badge + " " + st.bold.Render(m.clip(p.title, max(titleRoom, 1)))
	gap := max(1, l.inner-lipgloss.Width(title)-lipgloss.Width(right))
	head := fit.Render(title + strings.Repeat(" ", gap) + right)

	lines := []string{head, st.dim.Render(strings.Repeat(m.g.rule, l.inner))}
	if l.hasImg {
		if m.proto.IsGraphic() {
			for i := 0; i < l.imgRows; i++ {
				lines = append(lines, "")
			}
		} else {
			pad := strings.Repeat(" ", l.imgLeft)
			for _, ln := range strings.Split(m.blocksArt(l.imgCols, l.imgRows), "\n") {
				lines = append(lines, pad+ln)
			}
		}
		lines = append(lines, "")
	}
	p.vp.Height = l.vpH
	lines = append(lines, strings.Split(p.vp.View(), "\n")...)

	// buttons (clickable) and the scroll position
	copyBtn := zone.Mark(zPopCopy, st.btn.Render("Enter  Copy"))
	closeBtn := zone.Mark(zPopClose, st.btnAlt.Render("Esc  Close"))
	btns := copyBtn + " " + closeBtn
	if len(p.lines) > l.vpH {
		pct := st.dim.Render(fmt.Sprintf("%3.0f%%", p.vp.ScrollPercent()*100))
		if g := l.inner - lipgloss.Width(btns) - lipgloss.Width(pct); g > 0 {
			btns += strings.Repeat(" ", g) + pct
		}
	}
	if l.spacer > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, btns)

	for i, ln := range lines { // leave nothing wider than the box
		lines[i] = fit.Render(ln)
	}
	box := st.box.Width(l.boxW - 2).Render(strings.Join(lines, "\n"))
	return m.centerAt(zone.Mark(zPopBox, box), l.x0, l.y0)
}

// centerAt positions a block with its top-left corner at (x0, y0).
func (m *model) centerAt(block string, x0, y0 int) string {
	pad := strings.Repeat(" ", x0)
	ls := strings.Split(block, "\n")
	for i := range ls {
		ls[i] = pad + ls[i]
	}
	return strings.Repeat("\n", y0) + strings.Join(ls, "\n")
}

// --- popup input -----------------------------------------------------------------

func (m *model) keyView(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return m.closePopup()
	case "enter": // copy and close the application
		return m.choosePopup()
	}
	var cmd tea.Cmd
	m.popLayout() // make sure the viewport knows its size and content
	m.pop.vp, cmd = m.pop.vp.Update(msg)
	return cmd
}

func (m *model) scrollPopupWheel(msg tea.MouseMsg) tea.Cmd {
	var cmd tea.Cmd
	m.popLayout()
	m.pop.vp, cmd = m.pop.vp.Update(msg)
	return cmd
}

func (m *model) clickView(id string) tea.Cmd {
	switch id {
	case zPopCopy:
		return m.choosePopup()
	case zPopClose, "":
		return m.closePopup() // the buttons, or a click outside the box
	}
	return nil
}

func (m *model) choosePopup() tea.Cmd {
	res, err := m.resultFor(m.pop.clip)
	if err != nil {
		m.setFlash(err.Error())
		return m.closePopup()
	}
	_ = m.st.TouchClip(m.ctx, m.pop.clip.c.ID)
	m.result = res
	return tea.Quit
}

func (m *model) closePopup() tea.Cmd {
	m.overlay = overlayNone
	return m.clearImage()
}

// --- pictures drawn by the terminal itself (kitty, iTerm2, sixel) ------------------

// scheduleImage draws the picture shortly after the frame is on screen, so
// it never races Bubble Tea's own output.
func (m *model) scheduleImage() tea.Cmd {
	if m.overlay != overlayView || !m.proto.IsGraphic() || m.pop.img == nil || m.pop.drawn || !m.popLayout().hasImg {
		return nil
	}
	gen := m.pop.gen
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return drawImageMsg{gen} })
}

func (m *model) drawImage(msg drawImageMsg) {
	if msg.gen != m.pop.gen || m.overlay != overlayView || m.pop.drawn || m.out == nil {
		return
	}
	l := m.popLayout()
	if !l.hasImg {
		return
	}
	seq, err := preview.Graphic(m.proto, m.pop.img, l.imgCols, l.imgRows, m.cellW, m.cellH)
	if err != nil {
		m.pop.imgNote = err.Error()
		return
	}
	_, _ = io.WriteString(m.out, preview.Place(seq, m.pop.imgTop+1, m.pop.imgX+1))
	m.pop.drawn = true
}

// clearImage removes a drawn picture and makes Bubble Tea repaint the screen.
func (m *model) clearImage() tea.Cmd {
	if !m.pop.drawn {
		return nil
	}
	m.pop.drawn = false
	m.pop.gen++
	out, proto := m.out, m.proto
	return tea.Sequence(func() tea.Msg {
		if out != nil && proto == preview.Kitty {
			_, _ = io.WriteString(out, preview.KittyDelete())
		}
		return nil
	}, tea.ClearScreen)
}

// onResize refreshes the cell size and redraws a picture that is on screen.
func (m *model) onResize() tea.Cmd {
	if m.proto.IsGraphic() {
		m.cellW, m.cellH = preview.CellSize()
	}
	if m.overlay != overlayView {
		return nil
	}
	m.pop.lines = nil // rewrap for the new width
	wasDrawn := m.pop.drawn
	clear := m.clearImage()
	if wasDrawn {
		return tea.Sequence(clear, m.scheduleImage())
	}
	return m.scheduleImage()
}
