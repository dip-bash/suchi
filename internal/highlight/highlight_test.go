package highlight

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]`)

func strip(s string) string { return ansi.ReplaceAllString(s, "") }

func withProfile(t *testing.T, p termenv.Profile) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(p)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name, file, text, want string
	}{
		{"python", "", "def fibonacci(n):\n    a, b = 0, 1\n    for _ in range(n):\n        a, b = b, a + b\n    return a", "Python"},
		{"python import", "", "import os\nimport sys\n\nprint(os.getcwd())", "Python"},
		{"go", "", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}", "Go"},
		{"rust", "", "fn main() {\n    let mut x = 5;\n    println!(\"{}\", x);\n}", "Rust"},
		{"javascript", "", "const add = (a, b) => a + b;\nconsole.log(add(1, 2));", "JavaScript"},
		{"c", "", "#include <stdio.h>\nint main(void) {\n  printf(\"hi\");\n  return 0;\n}", "C"},
		{"c++", "", "#include <iostream>\nint main() {\n  std::cout << 1;\n}", "C++"},
		{"java", "", "public class Main {\n  public static void main(String[] a) {\n    System.out.println(1);\n  }\n}", "Java"},
		{"json", "", `{"name": "suchi", "tags": ["a", "b"], "n": 3}`, "JSON"},
		{"json array", "", "[1, 2, 3]", "JSON"},
		{"html", "", "<!DOCTYPE html>\n<html>\n<body><div>hi</div></body>\n</html>", "HTML"},
		{"xml", "", "<?xml version=\"1.0\"?>\n<root><item id=\"1\"/></root>", "XML"},
		{"yaml", "", "name: suchi\nversion: 1\nitems:\n  - a\n  - b", "YAML"},
		{"toml", "", "[package]\nname = \"suchi\"\nversion = \"0.5.0\"", "TOML"},
		{"css", "", "body {\n  margin: 0;\n  color: red;\n}", "CSS"},
		{"markdown", "", "# Title\n\nSome text with a [link](http://x.y).\n\n- one\n- two", "Markdown"},
		{"sql one-liner", "", "SELECT id, name FROM customers WHERE created_at > now() - interval '7 days' ORDER BY id;", "SQL"},
		{"sql insert", "", "INSERT INTO users (id, name) VALUES (1, 'a');", "SQL"},
		{"ssh command", "", "ssh -i ~/.ssh/id_ed25519 deploy@203.0.113.7 -p 2222", "Bash"},
		{"git command", "", "git commit -m \"fix: handle empty clipboard\"", "Bash"},
		{"prompt", "", "$ ls -la /tmp", "Bash"},
		{"env assignment", "", "FOO=bar make deploy", "Bash"},
		{"shebang bash", "", "#!/bin/bash\necho hi", "Bash"},
		{"shebang python", "", "#!/usr/bin/env python3\nprint(1)", "Python"},
		{"diff", "", "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new", "Diff"},
		{"dockerfile", "", "FROM golang:1.24\nRUN go build ./...\nCMD [\"./app\"]", "Docker"},
		{"by filename", "main.go", "x", "Go"},
		{"by filename py", "/home/u/script.py", "just text", "Python"},
		{"by filename Makefile", "/src/Makefile", "all:\n\tgo build", "Makefile"},
	}
	for _, c := range cases {
		got := Detect(c.file, c.text).Name()
		if got != c.want {
			t.Errorf("%s: detected %q, want %q", c.name, got, c.want)
		}
	}
}

func TestProseAndShortThingsStayPlain(t *testing.T) {
	for _, s := range []string{
		"me@example.com",
		"https://github.com/dip-bash/suchi/releases/latest",
		"The quick brown fox jumps over the lazy dog.",
		"Meeting at 3pm tomorrow, bring the slides and the laptop charger please.",
		"Hello, can you send me the invoice for last month? Thanks!",
		"Best regards,\nDip\nsent from suchi",
		"+1 (555) 010-9999",
		"1234",
		"ok",
		"Dear team,\n\nPlease find the report attached.\nLet me know if anything is unclear.\n\nThanks",
		"192.168.1.10",
		"",
		" ",
	} {
		if l := Detect("", s); l.Known() {
			t.Errorf("%q must stay plain text, detected %s", s, l.Name())
		}
	}
	if Detect("", "me@example.com").Badge() != "TEXT" {
		t.Error("plain text has the TEXT badge")
	}
}

func TestBadges(t *testing.T) {
	for in, want := range map[string]string{"python": "PYTHON", "javascript": "JS", "json": "JSON", "docker": "DOCKER", "markdown": "MD", "go": "GO"} {
		if got := lang(in).Badge(); got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeNeverLetsControlCharactersThrough(t *testing.T) {
	got := Sanitize("a\x1b[31mred\x1b[0m\x07bell\r\nnext\rlast\u200bzero\u202ertl\x00nul\x9bc1\ttab")
	for _, r := range got {
		if r != '\n' && r != '\t' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) {
			t.Fatalf("control character %q survived in %q", r, got)
		}
	}
	if strings.ContainsAny(got, "\r\u200b\u202e") || strings.Count(got, "\n") != 2 {
		t.Fatalf("got %q", got)
	}
	if Sanitize("ok \xff\xfe bytes") != "ok \uFFFD bytes" { // a run of bad bytes becomes one replacement
		t.Fatalf("invalid UTF-8 must be replaced, got %q", Sanitize("ok \xff\xfe bytes"))
	}
}

func TestRenderNumbersLines(t *testing.T) {
	withProfile(t, termenv.Ascii)
	r := Render("one\ntwo\nthree\n", lang("python"), Options{Width: 40, LineNumbers: true})
	want := []string{"  1 │ one", "  2 │ two", "  3 │ three"}
	if r.LineCount != 3 || len(r.Lines) != 3 {
		t.Fatalf("lines: %q", r.Lines)
	}
	for i, w := range want {
		if strip(r.Lines[i]) != w {
			t.Errorf("line %d = %q, want %q", i, strip(r.Lines[i]), w)
		}
	}
	// the gutter grows with the number of lines
	var sb strings.Builder
	for i := 0; i < 120; i++ {
		sb.WriteString("x\n")
	}
	r = Render(sb.String(), Lang{}, Options{Width: 40, LineNumbers: true})
	if got := strip(r.Lines[0]); got != "  1 │ x" {
		t.Errorf("3-digit gutter: %q", got)
	}
	if got := strip(r.Lines[119]); got != "120 │ x" {
		t.Errorf("last line: %q", got)
	}
	// ASCII gutter and no numbers
	r = Render("a\nb", Lang{}, Options{Width: 40, LineNumbers: true, ASCII: true})
	if strip(r.Lines[0]) != "  1 | a" {
		t.Errorf("ascii gutter: %q", strip(r.Lines[0]))
	}
	r = Render("a\nb", Lang{}, Options{Width: 40})
	if strip(r.Lines[0]) != "a" || len(r.Lines) != 2 {
		t.Errorf("no gutter: %q", r.Lines)
	}
}

func TestRenderColoursTokens(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	r := Render("def f(x):\n    return 'hi'  # note", lang("python"), Options{Width: 60, LineNumbers: true})
	if !r.Coloured {
		t.Fatal("expected colour")
	}
	joined := strings.Join(r.Lines, "\n")
	if !strings.Contains(joined, "\x1b[38;2;") {
		t.Fatalf("true-colour escapes expected: %q", joined)
	}
	// different token kinds get different colours: the keyword and the string literal
	colours := map[string]bool{}
	for _, m := range regexp.MustCompile(`\x1b\[38;2;(\d+;\d+;\d+)m`).FindAllStringSubmatch(joined, -1) {
		colours[m[1]] = true
	}
	if len(colours) < 3 {
		t.Fatalf("a highlighted snippet uses several colours, got %d", len(colours))
	}
	if strip(r.Lines[1]) != "  2 │     return 'hi'  # note" {
		t.Fatalf("highlighting must not change the text: %q", strip(r.Lines[1]))
	}
}

func TestRenderWithoutColourIsPlain(t *testing.T) {
	withProfile(t, termenv.Ascii)
	r := Render("def f(x):\n    return 1", lang("python"), Options{Width: 40, LineNumbers: true})
	if r.Coloured || ansi.MatchString(strings.Join(r.Lines, "")) && !strings.Contains(strings.Join(r.Lines, ""), "│") {
		t.Fatalf("no colour profile -> no colour: %q", r.Lines)
	}
	for _, l := range r.Lines {
		if strings.Contains(l, "\x1b[38") || strings.Contains(l, "\x1b[1m") {
			t.Fatalf("unexpected colour escape: %q", l)
		}
	}
}

func TestWrappingKeepsColoursAndTheGutterAligned(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	long := "x = '" + strings.Repeat("abcdefghij", 12) + "'"
	r := Render("a = 1\n"+long+"\nz = 2", lang("python"), Options{Width: 30, LineNumbers: true})
	if len(r.Lines) < 6 {
		t.Fatalf("the long line must wrap into several rows: %d rows", len(r.Lines))
	}
	var rows []string
	for _, l := range r.Lines {
		if w := runewidth.StringWidth(strip(l)); w > 30 {
			t.Errorf("row wider than 30: %d %q", w, strip(l))
		}
		rows = append(rows, strip(l))
	}
	if !strings.HasPrefix(rows[1], "  2 │ x = '") || !strings.HasPrefix(rows[2], "    │ ") {
		t.Errorf("continuation rows have a blank number: %q", rows[:3])
	}
	last := rows[len(rows)-1]
	if !strings.HasPrefix(last, "  3 │ z = 2") {
		t.Errorf("numbering continues with the next logical line: %q", last)
	}
	// continuation rows are still coloured as the string they belong to
	if !strings.Contains(r.Lines[2], "\x1b[38;2;") {
		t.Errorf("wrapped part lost its colour: %q", r.Lines[2])
	}
	// no text lost
	joined := ""
	for _, rw := range rows[1 : len(rows)-1] {
		joined += strings.TrimPrefix(strings.TrimPrefix(rw, "  2 │ "), "    │ ")
	}
	if joined != long {
		t.Errorf("wrapping changed the text:\n got  %q\n want %q", joined, long)
	}
}

func TestTabsAndWideCharacters(t *testing.T) {
	withProfile(t, termenv.Ascii)
	r := Render("\tindented\nab\tc\n日本語日本語日本語", Lang{}, Options{Width: 14, LineNumbers: true})
	rows := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		rows[i] = strip(l)
		if w := runewidth.StringWidth(rows[i]); w > 14 {
			t.Errorf("row wider than the box: %d %q", w, rows[i])
		}
		if strings.ContainsRune(rows[i], '\t') {
			t.Errorf("tabs must be expanded: %q", rows[i])
		}
	}
	if rows[0] != "  1 │     inde" { // 4 spaces from the tab, then wraps at the width
		t.Errorf("tab expansion: %q", rows[0])
	}
	if !strings.HasPrefix(rows[2], "  2 │ ab  c") { // tab stops: "ab" + 2 spaces
		t.Errorf("tab stop alignment: %q", rows[2])
	}
}

func TestEscapeSequencesInTextNeverReachTheOutput(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	r := Render("echo \x1b]0;pwned\x07 \x1b[2J done", lang("bash"), Options{Width: 60, LineNumbers: true})
	joined := strings.Join(r.Lines, "\n")
	for _, bad := range []string{"\x1b]0;", "\x1b[2J", "\x07"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("clipboard text injected %q into the terminal output", bad)
		}
	}
}

func TestHugeTextIsNumberedButNotHighlighted(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	big := strings.Repeat("x = 1\n", maxHighlightLines+10)
	r := Render(big, lang("python"), Options{Width: 40, LineNumbers: true})
	if r.Coloured || r.LineCount != maxHighlightLines+10 || len(r.Lines) != maxHighlightLines+10 {
		t.Fatalf("coloured=%v lines=%d rows=%d", r.Coloured, r.LineCount, len(r.Lines))
	}
}

func TestEmptyAndTinyWidths(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	if r := Render("", lang("go"), Options{Width: 20, LineNumbers: true}); len(r.Lines) != 1 {
		t.Fatalf("empty text renders one blank numbered row: %q", r.Lines)
	}
	r := Render("package main\nfunc main() {}", lang("go"), Options{Width: 6, LineNumbers: true}) // too narrow for a gutter
	for _, l := range r.Lines {
		if runewidth.StringWidth(strip(l)) > 6 {
			t.Errorf("too wide: %q", strip(l))
		}
	}
}
