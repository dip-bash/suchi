package tui

import (
	"fmt"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/dip-bash/suchi/internal/store"
)

const doubleClick = 450 * time.Millisecond

// Update implements tea.Model.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clamp(tabClips)
		m.clamp(tabSnips)
		cmd = m.onResize()
	case tickMsg:
		// The daemon runs in another process: refresh when it commits.
		if v, err := m.st.DataVersion(m.ctx); err == nil && v != m.dataVer {
			m.dataVer = v
			m.reload()
		}
		if m.flash != "" && time.Now().After(m.flashUntil) {
			m.flash = ""
		}
		cmd = tick()
	case frameMsg:
		m.pill.step()
		m.scroll.step()
		m.open.step()
		m.frameRunning = false
		if m.overlay == overlayView && m.open.settled() {
			cmd = m.scheduleImage()
		}
	case drawImageMsg:
		m.drawImage(msg)
	case execDoneMsg:
		if msg.err != nil {
			m.setFlash(fmt.Sprintf("%q: %v", msg.name, msg.err))
		} else {
			m.setFlash(fmt.Sprintf("ran %q", msg.name))
		}
	case tea.KeyMsg:
		cmd = m.key(msg)
	case tea.MouseMsg:
		cmd = m.mouse(msg)
	}
	return m, tea.Batch(cmd, m.kick())
}

func (m *model) key(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "ctrl+c" {
		return tea.Quit
	}
	m.flash = ""
	switch m.overlay {
	case overlayHelp:
		m.overlay = overlayNone // any key closes the help
		return nil
	case overlayView:
		return m.keyView(msg)
	case overlayForm:
		return m.keyForm(msg)
	}
	if m.search {
		return m.keySearch(msg)
	}
	return m.keyNormal(msg.String())
}

func (m *model) keyNormal(k string) tea.Cmd {
	if k != "d" {
		m.armed = 0
	}
	switch k {
	case "esc":
		return tea.Quit
	case "i":
		m.search = true
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "pgdown":
		m.move(m.listHeight())
	case "pgup":
		m.move(-m.listHeight())
	case "home":
		m.panes[m.tab].sel = 0
		m.clamp(m.tab)
	case "end":
		m.panes[m.tab].sel = len(m.rows[m.tab]) - 1
		m.clamp(m.tab)
	case "tab", "shift+tab":
		m.setTab(1 - m.tab)
	case "f":
		m.fuzzy = !m.fuzzy
		m.refilter(tabClips)
		m.refilter(tabSnips)
		if m.fuzzy {
			m.setFlash("fuzzy search on")
		} else {
			m.setFlash("fuzzy search off")
		}
	case "enter":
		return m.choose()
	case "d":
		m.remove()
	case "p":
		m.togglePin()
	case "e":
		return m.openEntry()
	case "a":
		if m.tab == tabSnips {
			m.openForm(nil)
		}
	case "r":
		return m.run()
	case "?":
		m.overlay = overlayHelp
	}
	return nil
}

func (m *model) keySearch(msg tea.KeyMsg) tea.Cmd {
	p := &m.panes[m.tab]
	switch msg.String() {
	case "esc":
		m.search = false
	case "enter":
		return m.choose()
	case "up":
		m.move(-1)
	case "down":
		m.move(1)
	default:
		if editKey(&p.query, msg, false) {
			m.refilter(m.tab)
			p.sel, p.top = 0, 0
		}
	}
	return nil
}

// --- mouse ----------------------------------------------------------------------

// zoneIDs are the BubbleZone regions marked in the views.
const (
	zTabClip  = "tab:clip"
	zTabSnip  = "tab:snip"
	zSearch   = "search"
	zPopCopy  = "pop:copy"
	zPopClose = "pop:close"
	zPopBox   = "pop:box"
	zFormName = "form:name"
	zFormBody = "form:body"
	zFormText = "form:text"
	zFormCmd  = "form:command"
	zFormSave = "form:save"
	zFormQuit = "form:cancel"
)

func zoneRow(i int) string { return fmt.Sprintf("row:%d", i) }

// hit returns the first of ids whose zone contains the mouse position.
func hit(msg tea.MouseMsg, ids ...string) string {
	for _, id := range ids {
		if z := zone.Get(id); z != nil && z.InBounds(msg) {
			return id
		}
	}
	return ""
}

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Action != tea.MouseActionPress {
		return nil
	}
	wheel := msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown
	if !wheel && msg.Button != tea.MouseButtonLeft && msg.Button != tea.MouseButtonRight {
		return nil
	}
	switch m.overlay {
	case overlayHelp:
		if !wheel {
			m.overlay = overlayNone
		}
		return nil
	case overlayView:
		if wheel {
			return m.scrollPopupWheel(msg)
		}
		return m.clickView(hit(msg, zPopCopy, zPopClose, zPopBox))
	case overlayForm:
		if !wheel {
			m.clickForm(hit(msg, zFormSave, zFormQuit, zFormName, zFormBody, zFormText, zFormCmd))
		}
		return nil
	}
	if wheel {
		if msg.Button == tea.MouseButtonWheelUp {
			m.move(-3)
		} else {
			m.move(3)
		}
		return nil
	}

	ids := []string{zTabClip, zTabSnip, zSearch}
	top := m.shownTop(m.tab)
	for i := top; i < top+m.listHeight() && i < len(m.rows[m.tab]); i++ {
		ids = append(ids, zoneRow(i))
	}
	id := hit(msg, ids...)
	return m.click(id, msg.Button == tea.MouseButtonRight)
}

// click performs what a click on a zone means. It is separate from hit
// testing so it can be tested without a terminal.
func (m *model) click(id string, right bool) tea.Cmd {
	double := id != "" && id == m.lastClickID && time.Since(m.lastClickAt) < doubleClick
	m.lastClickID, m.lastClickAt = id, time.Now()

	switch id {
	case "":
		return nil
	case zTabClip:
		m.setTab(tabClips)
	case zTabSnip:
		m.setTab(tabSnips)
	case zSearch:
		if double {
			m.search = true
		}
	default:
		var i int
		if _, err := fmt.Sscanf(id, "row:%d", &i); err != nil || i < 0 || i >= len(m.rows[m.tab]) {
			return nil
		}
		m.panes[m.tab].sel = i
		m.clamp(m.tab)
		m.armed = 0
		if right { // right click: the same as "e"
			return m.openEntry()
		}
		// click: copy; a command snippet runs instead
		if sn, ok := m.selectedSnip(); m.tab == tabSnips && ok && sn.s.Kind == store.KindCommand {
			return m.run()
		}
		return m.choose()
	}
	return nil
}

// --- actions --------------------------------------------------------------------

// resultFor loads everything needed to put a history entry back on the clipboard.
func (m *model) resultFor(c clipItem) (Result, error) {
	id := c.c.ID
	switch c.c.Kind {
	case store.ClipImage:
		data, mime, err := m.st.ClipData(m.ctx, id)
		if err != nil {
			return Result{}, err
		}
		return Result{Chosen: true, Kind: store.ClipImage, Text: c.c.Preview, MIME: mime, Data: data}, nil
	case store.ClipFile:
		text, err := m.st.ClipText(m.ctx, id)
		return Result{Chosen: true, Kind: store.ClipFile, Text: text, MIME: "text/uri-list"}, err
	}
	text, err := m.st.ClipText(m.ctx, id)
	return Result{Chosen: true, Kind: store.ClipText, Text: text}, err
}

// choose copies the highlighted entry and closes suchi.
func (m *model) choose() tea.Cmd {
	if m.tab == tabSnips {
		sn, ok := m.selectedSnip()
		if !ok {
			return nil
		}
		// What suchi copies is not "history": keep it out of the Clip tab.
		_ = m.st.IgnoreNext(m.ctx, sn.s.Body)
		m.result = Result{Chosen: true, Kind: store.ClipText, Text: sn.s.Body}
		return tea.Quit
	}
	c, ok := m.selectedClip()
	if !ok {
		return nil
	}
	res, err := m.resultFor(c)
	if err != nil {
		m.setFlash(err.Error())
		return nil
	}
	_ = m.st.TouchClip(m.ctx, c.c.ID)
	m.result = res
	return tea.Quit
}

// remove deletes a clip at once; a snippet needs a second "d" because it is
// user-written data that cannot be recovered from the clipboard.
func (m *model) remove() {
	id := m.selectedID(m.tab)
	if id == 0 {
		return
	}
	if m.tab == tabClips {
		if err := m.st.DeleteClip(m.ctx, id); err != nil {
			m.setFlash(err.Error())
			return
		}
		m.reload()
		m.setFlash("deleted")
		return
	}
	if m.armed != id {
		m.armed = id
		sn, _ := m.selectedSnip()
		m.setFlash(fmt.Sprintf("press d again to delete %q", sn.s.Name))
		return
	}
	m.armed = 0
	if err := m.st.DeleteSnippet(m.ctx, id); err != nil {
		m.setFlash(err.Error())
		return
	}
	m.reload()
	m.setFlash("deleted")
}

func (m *model) togglePin() {
	if m.tab != tabClips {
		return
	}
	id := m.selectedID(tabClips)
	if id == 0 {
		return
	}
	if err := m.st.TogglePin(m.ctx, id); err != nil {
		m.setFlash(err.Error())
		return
	}
	m.reload()
	for _, r := range m.rows[tabClips] {
		if c := m.clips[r.idx].c; c.ID == id {
			if c.Pinned {
				m.setFlash("pinned")
			} else {
				m.setFlash("unpinned")
			}
		}
	}
}

// openEntry is "e": a read-only popup with the details of a clipboard
// entry, or the edit form for a snippet.
func (m *model) openEntry() tea.Cmd {
	if m.tab == tabSnips {
		if sn, ok := m.selectedSnip(); ok {
			s := sn.s
			m.openForm(&s)
		}
		return nil
	}
	c, ok := m.selectedClip()
	if !ok {
		return nil
	}
	return m.openView(c)
}

// run executes the highlighted command snippet in the user's terminal.
func (m *model) run() tea.Cmd {
	if m.tab != tabSnips {
		return nil
	}
	sn, ok := m.selectedSnip()
	if !ok {
		return nil
	}
	if sn.s.Kind != store.KindCommand {
		m.setFlash("not a command - press e and set the type to command")
		return nil
	}
	// The command runs in its own shell; afterwards wait for Enter so its
	// output stays readable before suchi redraws.
	const wrapper = `sh -c "$1"; s=$?; printf '\n[exit %s] press Enter to return... ' "$s"; read _; exit "$s"`
	cmd := exec.Command("sh", "-c", wrapper, "suchi", sn.s.Body)
	name := sn.s.Name
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{name: name, err: err} })
}
