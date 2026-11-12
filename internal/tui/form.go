package tui

import (
	"errors"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/dip-bash/suchi/internal/store"
)

const (
	fieldName = iota
	fieldBody
	fieldKind
	fieldCount
)

// form is the add/edit dialog for a snippet.
type form struct {
	id    int64 // 0 = new snippet
	name  input
	body  input
	kind  string
	field int
	err   string
}

// openForm shows the dialog for a new snippet, or for editing sn.
func (m *model) openForm(sn *store.Snippet) {
	m.fm = form{kind: store.KindText}
	if sn != nil {
		m.fm.id, m.fm.kind = sn.ID, sn.Kind
		m.fm.name.set(sn.Name)
		m.fm.body.set(sn.Body)
	}
	m.overlay = overlayForm
}

func (m *model) keyForm(msg tea.KeyMsg) tea.Cmd {
	f := &m.fm
	switch k := msg.String(); k {
	case "esc":
		m.overlay = overlayNone
	case "enter":
		m.saveForm()
	case "tab", "down":
		f.field = (f.field + 1) % fieldCount
	case "shift+tab", "up":
		f.field = (f.field + fieldCount - 1) % fieldCount
	default:
		switch f.field {
		case fieldName:
			editKey(&f.name, msg, false)
		case fieldBody:
			editKey(&f.body, msg, true)
		case fieldKind:
			switch k {
			case " ", "left", "right", "h", "l":
				if f.kind == store.KindText {
					f.kind = store.KindCommand
				} else {
					f.kind = store.KindText
				}
			case "t":
				f.kind = store.KindText
			case "c":
				f.kind = store.KindCommand
			}
		}
	}
	return nil
}

// clickForm handles a click on one of the form's zones.
func (m *model) clickForm(id string) {
	f := &m.fm
	switch id {
	case zFormSave:
		m.saveForm()
	case zFormQuit:
		m.overlay = overlayNone
	case zFormName:
		f.field = fieldName
	case zFormBody:
		f.field = fieldBody
	case zFormText:
		f.field, f.kind = fieldKind, store.KindText
	case zFormCmd:
		f.field, f.kind = fieldKind, store.KindCommand
	case "":
		// a click outside every control does nothing
	}
}

func (m *model) saveForm() {
	f := &m.fm
	sn := store.Snippet{ID: f.id, Name: f.name.String(), Body: f.body.String(), Kind: f.kind}
	var err error
	id := f.id
	if f.id == 0 {
		id, err = m.st.AddSnippet(m.ctx, sn)
	} else {
		err = m.st.UpdateSnippet(m.ctx, sn)
	}
	if err != nil {
		f.err = strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": ")
		if !errors.Is(err, store.ErrInvalid) {
			f.err = err.Error()
		}
		return
	}
	m.overlay = overlayNone
	m.panes[tabSnips].query.set("") // make sure the saved snippet is visible
	m.reload()
	m.refilter(tabSnips)
	m.selectID(tabSnips, id)
	m.setFlash("saved")
}

func (m *model) viewForm() string {
	f := &m.fm
	st := m.styles
	boxW := max(10, min(m.w-2, 72))
	inner := boxW - 4
	fw := max(2, inner-7) // label column "Name   "
	title := "New snippet"
	if f.id != 0 {
		title = "Edit snippet"
	}
	label := func(i int, s string) string {
		if f.field == i {
			return st.label.Render(s)
		}
		return st.dim.Render(s)
	}
	radio := func(on bool, s string) string {
		if on {
			return st.bold.Render(m.g.radioOn + " " + s)
		}
		return st.dim.Render(m.g.radioOff + " " + s)
	}
	kind := zone.Mark(zFormText, radio(f.kind == store.KindText, "text")) + "   " +
		zone.Mark(zFormCmd, radio(f.kind == store.KindCommand, "command"))
	if f.field == fieldKind {
		kind = st.cursor.Render(" ") + kind
	} else {
		kind = " " + kind
	}
	field := func(in *input, i int) string {
		s := in.render(fw, f.field == i, st, m.g.nl)
		if pad := fw - lipgloss.Width(s); pad > 0 {
			s += st.field.Render(strings.Repeat("_", pad))
		}
		return s
	}
	rows := []string{
		st.bold.Render(title),
		"",
		zone.Mark(zFormName, label(fieldName, "Name   ")+field(&f.name, fieldName)),
		zone.Mark(zFormBody, label(fieldBody, "Text   ")+field(&f.body, fieldBody)),
		label(fieldKind, "Type  ") + kind,
		"",
	}
	switch {
	case f.err != "":
		rows = append(rows, st.err.Render(m.clip(f.err, inner)))
	case f.kind == store.KindCommand:
		rows = append(rows, st.dim.Render(m.clip("a command can be run with r, or by clicking it", inner)))
	default:
		rows = append(rows, "")
	}
	hint := "Tab next " + m.g.dot + " Ctrl+J new line"
	rows = append(rows, zone.Mark(zFormSave, st.btn.Render("Enter  Save"))+" "+
		zone.Mark(zFormQuit, st.btnAlt.Render("Esc  Cancel")),
		st.dim.Render(m.clip(hint, inner)))
	fit := lipgloss.NewStyle().MaxWidth(inner)
	for i, r := range rows {
		rows[i] = fit.Render(r)
	}
	box := st.box.Width(boxW - 2).MaxHeight(m.h).Render(strings.Join(rows, "\n"))
	x0 := max(0, (m.w-boxW)/2)
	y0 := max(0, (m.h-lipgloss.Height(box))/2)
	return m.centerAt(box, x0, y0)
}
