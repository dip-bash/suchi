package tui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
	"github.com/muesli/termenv"

	"github.com/dip-bash/suchi/internal/preview"
	"github.com/dip-bash/suchi/internal/store"
	"github.com/dip-bash/suchi/internal/textutil"
)

var limits = store.Limits{Count: 100, MaxText: 1 << 20, MaxMedia: 1 << 22, MediaBudget: 1 << 28}

func newTestModelProto(t *testing.T, proto preview.Protocol, clips ...string) (*model, *bytes.Buffer) {
	t.Helper()
	t.Setenv("LC_ALL", "C.UTF-8") // unicode glyphs; the ASCII layout is checked separately
	t.Setenv("TERM", "xterm-256color")
	st, err := store.Open(filepath.Join(t.TempDir(), "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, c := range clips {
		if _, err := st.AddItem(context.Background(), store.NewClip{Text: c}, limits); err != nil {
			t.Fatal(err)
		}
	}
	out := &bytes.Buffer{}
	m := newModel(context.Background(), st, Options{Protocol: proto, Out: out})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m, out
}

func newTestModel(t *testing.T, clips ...string) *model {
	m, _ := newTestModelProto(t, preview.Blocks, clips...)
	return m
}

var named = map[string]tea.KeyType{
	"enter": tea.KeyEnter, "esc": tea.KeyEsc, "tab": tea.KeyTab, "backspace": tea.KeyBackspace,
	"down": tea.KeyDown, "up": tea.KeyUp, "ctrl+u": tea.KeyCtrlU, "ctrl+w": tea.KeyCtrlW,
	"ctrl+j": tea.KeyCtrlJ, "space": tea.KeySpace, "end": tea.KeyEnd, "pgdown": tea.KeyPgDown,
}

// press sends one key and returns the command it produced.
func press(m *model, key string) tea.Cmd {
	var msg tea.KeyMsg
	if kt, ok := named[key]; ok {
		msg = tea.KeyMsg{Type: kt}
		if kt == tea.KeySpace {
			msg.Runes = []rune{' '}
		}
	} else {
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := m.Update(msg)
	return cmd
}

func typeText(m *model, s string) {
	for _, r := range s {
		if r == ' ' {
			press(m, "space")
		} else {
			press(m, string(r))
		}
	}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func addSnippet(t *testing.T, m *model, name, body string, command bool) {
	t.Helper()
	press(m, "a")
	typeText(m, name)
	press(m, "tab")
	typeText(m, body)
	if command {
		press(m, "tab")
		press(m, "c")
	}
	press(m, "enter")
	if m.overlay != overlayNone {
		t.Fatalf("form still open: %q", m.fm.err)
	}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]|\x1b\][^\a]*\a|\x1b_[^\x1b]*\x1b\\`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func view(m *model) string { m.settle(); return plain(m.View()) }

func pngBytes(w, h int, c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{c.R, uint8(x * 255 / w), uint8(y * 255 / h), 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func addImage(t *testing.T, m *model, name string, data []byte) {
	t.Helper()
	if _, err := m.st.AddItem(context.Background(), store.NewClip{Kind: store.ClipImage, Text: name, Mime: "image/png", Data: data}, limits); err != nil {
		t.Fatal(err)
	}
	m.reload()
}

func addFiles(t *testing.T, m *model, paths ...string) {
	t.Helper()
	if _, err := m.st.AddItem(context.Background(), store.NewClip{Kind: store.ClipFile, Text: strings.Join(paths, "\n"), Mime: "text/uri-list"}, limits); err != nil {
		t.Fatal(err)
	}
	m.reload()
}

// --- keyboard behaviour --------------------------------------------------------

func TestEnterCopiesHighlightedClipAndQuits(t *testing.T) {
	m := newTestModel(t, "first", "second")
	press(m, "j")
	cmd := press(m, "enter")
	if !isQuit(cmd) || m.result.Text != "first" || !m.result.Chosen || m.result.Kind != store.ClipText {
		t.Fatalf("result=%+v quit=%v", m.result, isQuit(cmd))
	}
}

func TestEscQuitsWithoutChoosing(t *testing.T) {
	m := newTestModel(t, "a")
	if cmd := press(m, "esc"); !isQuit(cmd) || m.result.Chosen {
		t.Fatalf("esc must quit without a result: %+v", m.result)
	}
}

func TestRemovedShortcutsAreInert(t *testing.T) {
	m := newTestModel(t, "a", "b", "c")
	for _, k := range []string{"q", "g", "G", "/", "y"} {
		if cmd := press(m, k); cmd != nil {
			t.Errorf("%q must do nothing, got a command", k)
		}
		if m.search || m.result.Chosen || m.panes[tabClips].sel != 0 {
			t.Errorf("%q changed state: search=%v result=%+v sel=%d", k, m.search, m.result, m.panes[tabClips].sel)
		}
	}
	press(m, "i")
	typeText(m, "ab")
	press(m, "ctrl+u")
	press(m, "ctrl+w")
	if got := m.panes[tabClips].query.String(); got != "ab" {
		t.Fatalf("query = %q, ctrl+u/ctrl+w must not edit", got)
	}
}

func TestSearchModeAndTabState(t *testing.T) {
	m := newTestModel(t, "apple pie", "banana split", "apple tart")
	press(m, "i")
	typeText(m, "apple")
	if n := len(m.rows[tabClips]); n != 2 {
		t.Fatalf("filtered rows = %d, want 2", n)
	}
	press(m, "esc")
	if m.search || len(m.rows[tabClips]) != 2 {
		t.Fatal("esc in search mode must only leave search mode")
	}
	press(m, "tab")
	if m.tab != tabSnips {
		t.Fatal("tab must switch to snippets")
	}
	press(m, "tab")
	if m.tab != tabClips || m.panes[tabClips].query.String() != "apple" {
		t.Fatal("each tab keeps its own query")
	}
}

// --- layout: header, arrow, names -----------------------------------------------

func TestHeaderIsClipSnipSearch(t *testing.T) {
	m := newTestModel(t, "one", "two")
	first := strings.Split(view(m), "\n")[0]
	for _, want := range []string{"Clip", "Snip", "Search", "___"} {
		if !strings.Contains(first, want) {
			t.Errorf("header %q lacks %q", first, want)
		}
	}
	all := view(m)
	if strings.Contains(all, "Clipboard") || strings.Contains(all, "Snippets") {
		t.Errorf("the tabs are named Clip and Snip only:\n%s", all)
	}
	press(m, "i")
	typeText(m, "tw")
	if first := strings.Split(view(m), "\n")[0]; !strings.Contains(first, "Search tw") {
		t.Errorf("typed text must appear in the header search field: %q", first)
	}
}

func TestSelectedRowHasAShortArrow(t *testing.T) {
	m := newTestModel(t, "alpha", "beta")
	lines := strings.Split(view(m), "\n")
	var arrows []string
	for _, l := range lines[2:] {
		if strings.Contains(l, "➜") {
			arrows = append(arrows, l)
		}
	}
	if len(arrows) != 1 || !strings.HasPrefix(arrows[0], " ➜ ") || !strings.Contains(arrows[0], "beta") {
		t.Fatalf("exactly the selected row (the newest, beta) needs an arrow: %q", arrows)
	}
	press(m, "j")
	for _, l := range strings.Split(view(m), "\n")[2:] {
		if strings.Contains(l, "alpha") && !strings.HasPrefix(l, " ➜ ") {
			t.Fatalf("arrow must follow the selection: %q", l)
		}
	}
}

func TestSnippetsNeverAppearInTheClipTab(t *testing.T) {
	m := newTestModel(t, "real clip")
	press(m, "tab")
	addSnippet(t, m, "mail", "me@example.com", false)
	if cmd := press(m, "enter"); !isQuit(cmd) || m.result.Text != "me@example.com" {
		t.Fatalf("result=%+v", m.result)
	}
	// The daemon sees the snippet text arrive on the clipboard after suchi copied it...
	if ok, _ := m.st.AddClip(context.Background(), "me@example.com", 100, 1<<20); ok {
		t.Fatal("...and must not record it as history")
	}
	m.reload()
	for _, c := range m.clips {
		if strings.Contains(string(c.disp), "me@example.com") {
			t.Fatal("a snippet leaked into the clip list")
		}
	}
}

// --- popup (viewport) -------------------------------------------------------------

func TestFullTextPopup(t *testing.T) {
	long := strings.Repeat("0123456789", 1500) + "\nTAIL"
	m := newTestModel(t, long)
	press(m, "e")
	if m.overlay != overlayView || m.pop.text != long {
		t.Fatalf("popup must hold the FULL text (overlay=%v len=%d)", m.overlay, len(m.pop.text))
	}
	if cmd := press(m, "esc"); cmd != nil || m.overlay != overlayNone {
		t.Fatal("esc closes the popup and does not quit")
	}
	press(m, "e")
	if cmd := press(m, "enter"); !isQuit(cmd) || m.result.Text != long {
		t.Fatal("enter in the popup copies the full text and quits")
	}
}

func TestPopupIsReadOnlyAndScrollsWithAViewport(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "line %02d\n", i)
	}
	m := newTestModel(t, sb.String())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 14})
	press(m, "e")
	m.settle()
	m.View()
	if m.pop.vp.YOffset != 0 {
		t.Fatal("popup must start at the top")
	}
	press(m, "j")
	press(m, "j")
	press(m, "down")
	if m.pop.vp.YOffset != 3 {
		t.Fatalf("j/j/down must scroll 3 lines, YOffset=%d", m.pop.vp.YOffset)
	}
	press(m, "k")
	if m.pop.vp.YOffset != 2 {
		t.Fatalf("k scrolls up, YOffset=%d", m.pop.vp.YOffset)
	}
	press(m, "pgdown")
	if m.pop.vp.YOffset <= 2 {
		t.Fatal("pgdown must scroll a page")
	}
	before := m.pop.text
	typeText(m, "xyz")
	press(m, "backspace")
	if m.pop.text != before {
		t.Fatal("the popup is read-only")
	}
	if !strings.Contains(view(m), "%") {
		t.Fatal("a scrollable popup shows its position")
	}
}

// --- snippets ------------------------------------------------------------------------

func TestAddSnippetAndSameNamesAllowed(t *testing.T) {
	m := newTestModel(t)
	press(m, "tab")
	addSnippet(t, m, "mail", "me@example.com", false)
	addSnippet(t, m, "mail", "work@example.com", false)
	addSnippet(t, m, "deploy", "make deploy", true)
	if n := len(m.rows[tabSnips]); n != 3 {
		t.Fatalf("snippets visible = %d, want 3", n)
	}
	all, _ := m.st.ListSnippets(context.Background())
	if all[0].Name != "deploy" || all[0].Kind != store.KindCommand {
		t.Fatalf("got %+v", all)
	}
}

func TestSnippetFormValidationKeepsFormOpen(t *testing.T) {
	m := newTestModel(t)
	press(m, "tab")
	press(m, "a")
	press(m, "enter")
	if m.overlay != overlayForm || m.fm.err == "" {
		t.Fatalf("empty form must show an error and stay open (err=%q)", m.fm.err)
	}
	if cmd := press(m, "esc"); cmd != nil || m.overlay != overlayNone {
		t.Fatal("esc cancels the form")
	}
}

func TestSnippetSearchIsByNameOnly(t *testing.T) {
	m := newTestModel(t)
	press(m, "tab")
	addSnippet(t, m, "mail", "contains-the-word-deploy", false)
	addSnippet(t, m, "deploy", "x", false)
	press(m, "i")
	typeText(m, "deploy")
	if n := len(m.rows[tabSnips]); n != 1 || m.snips[m.rows[tabSnips][0].idx].s.Name != "deploy" {
		t.Fatalf("search must match names only, got %d rows", n)
	}
}

func TestEditSnippetWithE(t *testing.T) {
	m := newTestModel(t)
	press(m, "tab")
	addSnippet(t, m, "mail", "old@example.com", false)
	press(m, "e")
	if m.overlay != overlayForm || m.fm.name.String() != "mail" || m.fm.body.String() != "old@example.com" {
		t.Fatalf("form not prefilled: %+v", m.fm)
	}
	press(m, "tab")
	for i := 0; i < len("old@example.com"); i++ {
		press(m, "backspace")
	}
	typeText(m, "new@example.com")
	press(m, "enter")
	if got, _ := m.st.FindSnippets(context.Background(), "mail"); len(got) != 1 || got[0].Body != "new@example.com" {
		t.Fatalf("edit not saved: %+v", got)
	}
}

func TestDeletingASnippetNeedsTwoPresses(t *testing.T) {
	m := newTestModel(t)
	press(m, "tab")
	addSnippet(t, m, "a", "1", false)
	addSnippet(t, m, "b", "2", false)
	press(m, "d")
	if len(m.rows[tabSnips]) != 2 || !strings.Contains(m.flash, "again") {
		t.Fatalf("first d must only arm (flash=%q)", m.flash)
	}
	press(m, "j")
	press(m, "d")
	if len(m.rows[tabSnips]) != 2 {
		t.Fatal("d on a different snippet must re-arm, not delete")
	}
	press(m, "d")
	if len(m.rows[tabSnips]) != 1 {
		t.Fatalf("second d must delete, rows=%d", len(m.rows[tabSnips]))
	}
}

func TestDeleteAndPinClips(t *testing.T) {
	m := newTestModel(t, "one", "two")
	press(m, "j")
	press(m, "p")
	if !m.clips[m.rows[tabClips][0].idx].c.Pinned {
		t.Fatal("pinned entry must sort first")
	}
	press(m, "d")
	press(m, "d")
	if len(m.rows[tabClips]) != 1 {
		t.Fatalf("d d deletes a clip, rows=%d", len(m.rows[tabClips]))
	}
}

func TestRunOnlyForCommands(t *testing.T) {
	m := newTestModel(t)
	press(m, "tab")
	addSnippet(t, m, "mail", "me@example.com", false)
	if cmd := press(m, "r"); cmd != nil || !strings.Contains(m.flash, "not a command") {
		t.Fatalf("r on a text snippet must refuse (flash=%q)", m.flash)
	}
	addSnippet(t, m, "zz", "echo hi", true)
	press(m, "end")
	if cmd := press(m, "r"); cmd == nil {
		t.Fatal("r on a command snippet must return an exec command")
	}
}

func TestSnippetKeysOnlyOnSnippetTab(t *testing.T) {
	m := newTestModel(t, "x")
	press(m, "a")
	if m.overlay != overlayNone {
		t.Fatal("a must only work on the snippets tab")
	}
	if cmd := press(m, "r"); cmd != nil {
		t.Fatal("r must only work on the snippets tab")
	}
}

// --- media -----------------------------------------------------------------------------

func TestImageEntryRowBadgeAndCopy(t *testing.T) {
	m := newTestModel(t, "text clip")
	data := pngBytes(64, 32, color.NRGBA{R: 200})
	addImage(t, m, "screenshot-2026-11-07.png", data)
	press(m, "home")
	v := view(m)
	if !strings.Contains(v, "screenshot-2026-11-07.png") || !strings.Contains(v, " PNG ") {
		t.Fatalf("the row shows the file name and a PNG type badge:\n%s", v)
	}
	if cmd := press(m, "enter"); !isQuit(cmd) || m.result.Kind != store.ClipImage ||
		m.result.MIME != "image/png" || !bytes.Equal(m.result.Data, data) {
		t.Fatalf("choosing an image must return its bytes: %+v", m.result.Kind)
	}
}

func TestImagePopupShowsHalfBlockArt(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	m := newTestModel(t)
	addImage(t, m, "pic.png", pngBytes(120, 60, color.NRGBA{R: 255}))
	press(m, "e")
	if m.pop.img == nil {
		t.Fatalf("image must be decoded (note: %q)", m.pop.imgNote)
	}
	m.settle()
	v := plain(m.View())
	if !strings.Contains(v, "▀") || !strings.Contains(v, "pic.png") || !strings.Contains(v, "120 × 60") || !strings.Contains(v, "image/png") {
		t.Fatalf("popup lacks art or details:\n%s", v)
	}
	if out := strings.Count(v, "\n") + 1; out > m.h {
		t.Fatalf("popup is %d lines for a %d line terminal", out, m.h)
	}
}

func TestGraphicsProtocolDrawsInsideTheBoxAfterAFrame(t *testing.T) {
	m, out := newTestModelProto(t, preview.Kitty)
	addImage(t, m, "pic.png", pngBytes(160, 80, color.NRGBA{B: 255}))
	cmd := press(m, "e")
	if cmd == nil {
		t.Fatal("opening a picture popup must schedule the drawing")
	}
	if out.Len() != 0 {
		t.Fatal("nothing may be written before the frame is on screen")
	}
	v := m.View() // the frame: picture area is blank, no art characters
	if strings.Contains(v, "▀") {
		t.Fatal("graphics protocols must not paint half-blocks")
	}
	_, _ = m.Update(drawImageMsg{gen: m.pop.gen})
	seq := out.String()
	re := regexp.MustCompile(`^\x1b7\x1b\[(\d+);(\d+)H\x1b_Ga=T,f=100,t=d,c=(\d+),r=(\d+),`)
	g := re.FindStringSubmatch(seq)
	if g == nil || !strings.HasSuffix(seq, "\x1b8") {
		t.Fatalf("not a placed kitty image: %.80q", seq)
	}
	var row, col, cols, rows int
	fmt.Sscan(g[1], &row)
	fmt.Sscan(g[2], &col)
	fmt.Sscan(g[3], &cols)
	fmt.Sscan(g[4], &rows)
	lines := strings.Split(plain(v), "\n")
	// the whole picture area (1-based row..row+rows-1, col..col+cols-1) must be blank cells inside the box borders
	for r := row - 1; r < row-1+rows; r++ {
		cells := []rune(lines[r])
		if cells[col-2] != ' ' && cells[col-2] != '│' || len(cells) < col-1+cols {
			t.Fatalf("picture row %d does not fit inside the box: %q", r, lines[r])
		}
		if strings.TrimSpace(string(cells[col-1:col-1+cols])) != "" {
			t.Fatalf("picture area must be reserved blank, row %d: %q", r, lines[r])
		}
	}
	if !strings.Contains(lines[row-2], "─") {
		t.Fatalf("the picture must start right under the title rule: %q", lines[row-2])
	}
	// closing deletes the kitty image and repaints
	out.Reset()
	cmd = press(m, "esc")
	if cmd == nil {
		t.Fatal("closing a drawn picture must clean up")
	}
	runAll(cmd) // runs the sequence: delete + clear
	if !strings.Contains(out.String(), preview.KittyDelete()) {
		t.Fatalf("kitty image not deleted: %q", out.String())
	}
}

func TestStaleDrawMessagesAreIgnored(t *testing.T) {
	m, out := newTestModelProto(t, preview.Sixel)
	addImage(t, m, "pic.png", pngBytes(80, 40, color.NRGBA{G: 255}))
	press(m, "e")
	gen := m.pop.gen
	press(m, "esc") // closed before the timer fired
	_, _ = m.Update(drawImageMsg{gen: gen})
	if out.Len() != 0 {
		t.Fatal("a late draw message must not paint over the list")
	}
}

func TestFileEntriesShowNameBadgeAndPreview(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "photo.png")
	vidPath := filepath.Join(dir, "demo.mp4")
	_ = os.WriteFile(imgPath, pngBytes(64, 64, color.NRGBA{R: 90}), 0o600)
	_ = os.WriteFile(vidPath, make([]byte, 2048), 0o600)

	m := newTestModel(t)
	addFiles(t, m, vidPath)
	v := view(m)
	if !strings.Contains(v, "demo.mp4") || !strings.Contains(v, " MP4 ") {
		t.Fatalf("file row needs its name and an MP4 badge:\n%s", v)
	}
	press(m, "e")
	pv := view(m)
	if m.pop.img != nil || !strings.Contains(pv, "2.0 KB") || !strings.Contains(pv, "MP4") {
		t.Fatalf("a video gets a details card, no picture:\n%s", pv)
	}
	press(m, "esc")

	addFiles(t, m, imgPath, vidPath)
	press(m, "home")
	if v := view(m); !strings.Contains(v, "+1") {
		t.Fatalf("several files show a +N counter:\n%s", v)
	}
	press(m, "e")
	if m.pop.img == nil || !strings.Contains(view(m), "2 copied files") {
		t.Fatalf("the first file is a picture and must be previewed (note %q)", m.pop.imgNote)
	}
	if cmd := press(m, "enter"); !isQuit(cmd) || m.result.Kind != store.ClipFile ||
		m.result.Text != imgPath+"\n"+vidPath {
		t.Fatalf("choosing files returns their paths: %+v", m.result)
	}
}

func TestMissingFileIsReported(t *testing.T) {
	m := newTestModel(t)
	addFiles(t, m, "/definitely/not/here/ghost.pdf")
	press(m, "e")
	if v := view(m); !strings.Contains(v, "ghost.pdf") || !strings.Contains(v, "missing") || !strings.Contains(v, "PDF") {
		t.Fatalf("got:\n%s", v)
	}
}

func TestPreviewCanBeTurnedOff(t *testing.T) {
	m, _ := newTestModelProto(t, preview.None)
	addImage(t, m, "pic.png", pngBytes(40, 40, color.NRGBA{}))
	press(m, "e")
	if m.pop.img != nil || !strings.Contains(view(m), "previews are turned off") {
		t.Fatal("SUCHI_IMAGE=none must show details only")
	}
}

// --- mouse ----------------------------------------------------------------------------

func TestClickActions(t *testing.T) {
	m := newTestModel(t, "first", "second")
	press(m, "tab")
	addSnippet(t, m, "deploy", "echo hi", true)
	addSnippet(t, m, "mail", "me@example.com", false)
	press(m, "tab")

	// clicking a tab switches to it
	m.click(zTabSnip, false)
	if m.tab != tabSnips {
		t.Fatal("click on the Snip tab")
	}
	m.click(zTabClip, false)
	if m.tab != tabClips {
		t.Fatal("click on the Clip tab")
	}

	// a single click on a text row copies it and quits
	if cmd := m.click(zoneRow(1), false); !isQuit(cmd) || m.result.Text != "first" {
		t.Fatalf("click copies: %+v", m.result)
	}

	// snippets: a command runs, a text copies
	m.result = Result{}
	m.setTab(tabSnips)
	if cmd := m.click(zoneRow(0), false); cmd == nil || isQuit(cmd) || m.result.Chosen {
		t.Fatalf("clicking a command snippet runs it (must not quit): cmd=%v", cmd)
	}
	if cmd := m.click(zoneRow(1), false); !isQuit(cmd) || m.result.Text != "me@example.com" {
		t.Fatalf("clicking a text snippet copies it: %+v", m.result)
	}

	// right click = e
	m.result = Result{}
	m.setTab(tabClips)
	if cmd := m.click(zoneRow(0), true); cmd != nil || m.overlay != overlayView {
		t.Fatalf("right click opens the popup (overlay=%v)", m.overlay)
	}
	if cmd := m.clickView(zPopClose); cmd != nil || m.overlay != overlayNone {
		t.Fatal("Close button")
	}
	m.click(zoneRow(0), true)
	if cmd := m.clickView(""); m.overlay != overlayNone || cmd != nil {
		t.Fatal("a click outside the popup closes it")
	}
	m.click(zoneRow(0), true)
	if cmd := m.clickView(zPopCopy); !isQuit(cmd) {
		t.Fatal("Copy button copies and quits")
	}
	// rows that do not exist are ignored
	if cmd := m.click(zoneRow(99), false); cmd != nil {
		t.Fatal("clicking beyond the list does nothing")
	}
}

func TestDoubleClickOnSearchStartsTyping(t *testing.T) {
	m := newTestModel(t, "x")
	m.click(zSearch, false)
	if m.search {
		t.Fatal("a single click must not start search")
	}
	m.click(zSearch, false)
	if !m.search {
		t.Fatal("double click starts search")
	}
	m.search = false
	m.lastClickID = ""
	m.click(zSearch, false)
	m.lastClickAt = time.Now().Add(-time.Second) // too slow to be a double click
	m.click(zSearch, false)
	if m.search {
		t.Fatal("two slow clicks are not a double click")
	}
}

func TestFormClicks(t *testing.T) {
	m := newTestModel(t)
	press(m, "tab")
	press(m, "a")
	m.clickForm(zFormBody)
	if m.fm.field != fieldBody {
		t.Fatal("click focuses the text field")
	}
	m.clickForm(zFormCmd)
	if m.fm.kind != store.KindCommand || m.fm.field != fieldKind {
		t.Fatal("click selects the command type")
	}
	m.clickForm(zFormText)
	if m.fm.kind != store.KindText {
		t.Fatal("click selects the text type")
	}
	m.fm.name.set("n")
	m.fm.body.set("b")
	m.clickForm(zFormSave)
	if m.overlay != overlayNone || len(m.snips) != 1 {
		t.Fatal("the Save button saves")
	}
	press(m, "a")
	m.clickForm(zFormQuit)
	if m.overlay != overlayNone {
		t.Fatal("the Cancel button closes")
	}
}

// The real path: render, let BubbleZone record the zones, then click where the zone is.
func TestMouseHitTestingThroughZones(t *testing.T) {
	m := newTestModel(t, "alpha", "beta", "gamma")
	m.settle()
	m.View()
	var z *zone.ZoneInfo
	for i := 0; i < 100 && (z == nil || z.IsZero()); i++ {
		time.Sleep(5 * time.Millisecond)
		z = zone.Get(zTabSnip)
	}
	if z == nil || z.IsZero() {
		t.Fatal("BubbleZone never reported the Snip tab")
	}
	m.Update(tea.MouseMsg{X: z.StartX, Y: z.StartY, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.tab != tabSnips {
		t.Fatal("a real click on the Snip tab must switch to it")
	}

	m.setTab(tabClips)
	m.settle()
	m.View()
	var r *zone.ZoneInfo
	for i := 0; i < 100 && (r == nil || r.IsZero()); i++ {
		time.Sleep(5 * time.Millisecond)
		r = zone.Get(zoneRow(1))
	}
	if r == nil || r.IsZero() {
		t.Fatal("no zone for row 1")
	}
	_, cmd := m.Update(tea.MouseMsg{X: r.StartX + 4, Y: r.StartY, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if !isQuitBatch(cmd) || m.result.Text != "beta" {
		t.Fatalf("a real click on row 1 must copy it: %+v", m.result)
	}
}

// runAll executes a command and, recursively, every command inside the
// Batch / Sequence messages it returns.
func runAll(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	v := reflect.ValueOf(cmd())
	if v.Kind() == reflect.Slice {
		for i := 0; i < v.Len(); i++ {
			if c, ok := v.Index(i).Interface().(tea.Cmd); ok {
				runAll(c)
			}
		}
	}
}

// isQuitBatch finds a quit inside a Batch/Sequence (Update batches with the animation kick).
func isQuitBatch(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case tea.QuitMsg:
		return true
	case tea.BatchMsg:
		for _, c := range msg {
			if isQuitBatch(c) {
				return true
			}
		}
	}
	return false
}

func TestWheelMovesTheSelection(t *testing.T) {
	m := newTestModel(t, "1", "2", "3", "4", "5", "6", "7")
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if m.panes[tabClips].sel != 3 {
		t.Fatalf("wheel down moves 3 rows, sel=%d", m.panes[tabClips].sel)
	}
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.panes[tabClips].sel != 0 {
		t.Fatalf("wheel up, sel=%d", m.panes[tabClips].sel)
	}
}

// --- animation ---------------------------------------------------------------------------

func TestTabSwitchIsInstant(t *testing.T) {
	m := newTestModel(t, "x")
	press(m, "tab")
	if m.pill.pos != 7 || m.animating() {
		t.Fatalf("switching tabs must not animate (pos=%v animating=%v)", m.pill.pos, m.animating())
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab}); cmd != nil && m.frameRunning {
		t.Fatal("a tab switch must not start the frame loop")
	}
	if m.pill.pos != 0 {
		t.Fatalf("back to Clip at once, pos=%v", m.pill.pos)
	}
	m.click(zTabSnip, false)
	if m.pill.pos != 7 || m.animating() {
		t.Fatal("clicking a tab is instant too")
	}
}

func TestFrameLoopStopsWhenSettled(t *testing.T) {
	m := newTestModel(t, strings.Repeat("a line\n", 20))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}) // the popup unfolds with a spring
	if cmd == nil || !m.frameRunning {
		t.Fatal("an animation needs the frame loop")
	}
	for i := 0; i < 600 && m.frameRunning; i++ {
		m.Update(frameMsg(time.Now()))
	}
	if m.frameRunning || m.animating() {
		t.Fatal("the frame loop must stop once everything has settled (no idle CPU use)")
	}
}

func TestListGlidesButKeepsTheSelectionVisible(t *testing.T) {
	var clips []string
	for i := 0; i < 60; i++ {
		clips = append(clips, fmt.Sprintf("entry %02d", i))
	}
	m := newTestModel(t, clips...)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m.settle()
	press(m, "end")
	m.syncAnim()
	sel, h := m.panes[tabClips].sel, m.listHeight()
	for i := 0; i < 600 && m.animating(); i++ {
		top := m.shownTop(tabClips)
		if sel < top || sel >= top+h {
			t.Fatalf("selection %d scrolled out of view (top=%d)", sel, top)
		}
		m.scroll.step()
	}
	if got := m.shownTop(tabClips); got != m.panes[tabClips].top {
		t.Fatalf("list must settle on the real position: %d vs %d", got, m.panes[tabClips].top)
	}
}

func TestPopupUnfolds(t *testing.T) {
	m := newTestModel(t, strings.Repeat("a line\n", 30))
	press(m, "e")
	first := m.popLayout().boxH
	m.settle()
	if last := m.popLayout().boxH; last <= first {
		t.Fatalf("the popup must grow while opening: %d -> %d", first, last)
	}
}

// --- every screen fits the terminal ------------------------------------------------------------

func TestEveryViewFitsTheTerminal(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "p.png")
	_ = os.WriteFile(imgPath, pngBytes(300, 150, color.NRGBA{R: 40}), 0o600)

	m := newTestModel(t, "short", strings.Repeat("日本語 wide and long ", 60), "line1\nline2\nline3")
	addImage(t, m, "a-very-long-picture-name-that-will-not-fit-in-a-narrow-terminal.png", pngBytes(200, 100, color.NRGBA{R: 9}))
	addFiles(t, m, imgPath, "/some/very/long/path/to/a/file/with/a/long/name/demo.mp4")
	press(m, "tab")
	addSnippet(t, m, "name", strings.Repeat("body ", 80), true)
	press(m, "tab")

	check := func(label string, size [2]int) {
		t.Helper()
		m.settle()
		out := m.View()
		lines := strings.Split(out, "\n")
		if len(lines) > size[1] {
			t.Errorf("%s %v: %d lines for height %d", label, size, len(lines), size[1])
		}
		for _, l := range lines {
			if w := textutil.Width(plain(l)); w > size[0] {
				t.Errorf("%s %v: line width %d > %d: %q", label, size, w, size[0], plain(l))
				break
			}
		}
	}
	for _, ascii := range []bool{false, true} {
		if ascii {
			m.g = asciiGlyphs
		}
		for _, size := range [][2]int{{20, 6}, {30, 9}, {50, 12}, {80, 24}, {140, 50}} {
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			for tab := 0; tab < 2; tab++ {
				m.setTab(tabID(tab))
				check("list", size)
				press(m, "?")
				check("help", size)
				press(m, "x")
			}
			m.setTab(tabClips)
			for i := 0; i < len(m.rows[tabClips]); i++ { // a popup for every kind of entry
				m.panes[tabClips].sel = i
				press(m, "e")
				check(fmt.Sprintf("popup %d", i), size)
				press(m, "esc")
			}
			m.setTab(tabSnips)
			press(m, "a")
			check("form", size)
			press(m, "esc")
			m.setTab(tabClips)
		}
	}
}

// --- delete needs two presses, u undoes -------------------------------------------------

func rowTexts(m *model) []string {
	var out []string
	for _, r := range m.rows[tabClips] {
		out = append(out, string(m.clips[r.idx].disp))
	}
	return out
}

func TestEveryDeleteNeedsTwoPresses(t *testing.T) {
	m := newTestModel(t, "one", "two")
	press(m, "d")
	if len(m.rows[tabClips]) != 2 || !strings.Contains(m.flash, "press d again") || !strings.Contains(m.flash, "two") {
		t.Fatalf("a single d must only ask (flash=%q rows=%d)", m.flash, len(m.rows[tabClips]))
	}
	press(m, "j") // anything else disarms
	press(m, "d")
	if len(m.rows[tabClips]) != 2 {
		t.Fatal("after moving, d re-arms the new row instead of deleting")
	}
	press(m, "p") // another key in between disarms as well
	press(m, "d")
	if len(m.rows[tabClips]) != 2 {
		t.Fatal("d, p, d must not delete")
	}
	press(m, "d")
	if len(m.rows[tabClips]) != 1 || !strings.Contains(m.flash, "u to undo") {
		t.Fatalf("d d deletes and says how to undo (flash=%q)", m.flash)
	}
	// a click in between disarms too
	press(m, "d")
	m.click(zoneRow(0), true)
	m.overlay = overlayNone
	if m.armed != (armed{}) {
		t.Fatal("clicking disarms")
	}
}

func TestUndoRestoresAClipInPlace(t *testing.T) {
	m := newTestModel(t, "a", "b", "c") // list order: c, b, a
	press(m, "j")                       // on b
	press(m, "d")
	press(m, "d")
	if got := rowTexts(m); len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Fatalf("after delete: %v", got)
	}
	press(m, "u")
	if got := rowTexts(m); len(got) != 3 || got[0] != "c" || got[1] != "b" || got[2] != "a" {
		t.Fatalf("undo must put b back in the same place: %v", got)
	}
	if c, _ := m.selectedClip(); string(c.disp) != "b" {
		t.Fatalf("the restored entry is highlighted, got %q", c.disp)
	}
	if !strings.Contains(m.flash, "restored") {
		t.Fatalf("flash=%q", m.flash)
	}
	if len(m.undo) != 0 {
		t.Fatal("the undo entry is used up")
	}
}

func TestUndoKeepsPinAndImageBytes(t *testing.T) {
	m := newTestModel(t, "plain")
	data := pngBytes(50, 30, color.NRGBA{R: 7})
	addImage(t, m, "keep.png", data)
	press(m, "home")
	press(m, "p") // pin the image
	press(m, "d")
	press(m, "d")
	if len(m.rows[tabClips]) != 1 {
		t.Fatal("image should be gone")
	}
	press(m, "u")
	c, ok := m.selectedClip()
	if !ok || c.c.Kind != store.ClipImage || !c.c.Pinned || string(c.disp) != "keep.png" {
		t.Fatalf("restored entry: %+v", c.c)
	}
	got, mime, err := m.st.ClipData(context.Background(), c.c.ID)
	if err != nil || !bytes.Equal(got, data) || mime != "image/png" {
		t.Fatalf("image bytes must come back intact (err=%v)", err)
	}
	if m.clips[m.rows[tabClips][0].idx].c.ID != c.c.ID {
		t.Fatal("a pinned entry returns to the top")
	}
}

func TestUndoIsNewestFirstAndRepeatable(t *testing.T) {
	m := newTestModel(t, "a", "b", "c")
	for i := 0; i < 3; i++ { // delete c, b, a in turn
		press(m, "d")
		press(m, "d")
	}
	if len(m.rows[tabClips]) != 0 || len(m.undo) != 3 {
		t.Fatalf("rows=%d undo=%d", len(m.rows[tabClips]), len(m.undo))
	}
	press(m, "u")
	if got := rowTexts(m); len(got) != 1 || got[0] != "a" {
		t.Fatalf("the newest delete (a) comes back first: %v", got)
	}
	press(m, "u")
	press(m, "u")
	if got := rowTexts(m); len(got) != 3 || got[0] != "c" || got[1] != "b" || got[2] != "a" {
		t.Fatalf("three undos restore everything in the original order: %v", got)
	}
	press(m, "u")
	if m.flash != "nothing to undo" {
		t.Fatalf("flash=%q", m.flash)
	}
}

func TestUndoSnippetSwitchesToItsTab(t *testing.T) {
	m := newTestModel(t, "clip")
	press(m, "tab")
	addSnippet(t, m, "mail", "me@example.com", false)
	press(m, "d")
	press(m, "d")
	if len(m.rows[tabSnips]) != 0 {
		t.Fatal("snippet should be gone")
	}
	press(m, "tab") // look at the clips, then undo
	press(m, "u")
	if m.tab != tabSnips || len(m.rows[tabSnips]) != 1 {
		t.Fatalf("undo shows where the snippet came back (tab=%d rows=%d)", m.tab, len(m.rows[tabSnips]))
	}
	if sn, _ := m.selectedSnip(); sn.s.Name != "mail" || sn.s.Body != "me@example.com" {
		t.Fatalf("got %+v", sn.s)
	}
}

func TestUndoDoesNotDuplicateWhatWasCopiedAgain(t *testing.T) {
	m := newTestModel(t, "x", "again")
	press(m, "d")
	press(m, "d")                                                                      // delete "again"
	if _, err := m.st.AddClip(context.Background(), "again", 100, 1<<20); err != nil { // the daemon sees it copied once more
		t.Fatal(err)
	}
	m.reload()
	press(m, "u")
	n := 0
	for _, s := range rowTexts(m) {
		if s == "again" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d copies of the text after undo, want 1: %v", n, rowTexts(m))
	}
}

func TestUKeyTypesWhileSearching(t *testing.T) {
	m := newTestModel(t, "unicorn", "other")
	press(m, "d")
	press(m, "d")
	press(m, "i")
	typeText(m, "u")
	if m.panes[tabClips].query.String() != "u" || len(m.undo) != 1 {
		t.Fatalf("in search mode u is a letter: query=%q undo=%d", m.panes[tabClips].query.String(), len(m.undo))
	}
}

func TestUndoReportsEntriesHiddenBySearch(t *testing.T) {
	m := newTestModel(t, "apple", "banana")
	press(m, "d") // arm banana (the newest)
	press(m, "d")
	press(m, "i")
	typeText(m, "app")
	press(m, "esc")
	press(m, "u")
	if !strings.Contains(m.flash, "hidden by your search") {
		t.Fatalf("flash=%q", m.flash)
	}
	m.panes[tabClips].query.set("")
	m.refilter(tabClips)
	if len(m.rows[tabClips]) != 2 {
		t.Fatal("banana must be back")
	}
}

func TestUndoStackIsBounded(t *testing.T) {
	var clips []string
	for i := 0; i < maxUndo+5; i++ {
		clips = append(clips, fmt.Sprintf("entry %03d", i))
	}
	m := newTestModel(t, clips...)
	for i := 0; i < maxUndo+5; i++ {
		press(m, "d")
		press(m, "d")
	}
	if len(m.undo) != maxUndo {
		t.Fatalf("undo history is capped at %d, got %d", maxUndo, len(m.undo))
	}
}

func TestFooterOffersUndoOnlyWhenThereIsSomething(t *testing.T) {
	m := newTestModel(t, "a", "b")
	if strings.Contains(view(m), "undo") {
		t.Fatal("no undo hint before any delete")
	}
	press(m, "d")
	press(m, "d")
	m.flash = ""
	if !strings.Contains(view(m), "u undo") {
		t.Fatalf("after a delete the footer offers undo:\n%s", view(m))
	}
}
