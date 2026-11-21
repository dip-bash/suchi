// Package highlight renders text the way `bat` does: syntax highlighted, with
// a line-number gutter, wrapped to a width. The language is detected from the
// file name, a shebang, or the content itself.
//
// Clipboard text is untrusted, so it is sanitised first: control characters
// (including ESC) never reach the terminal.
package highlight

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

const (
	tabWidth  = 4
	minDigits = 3
	// Above these sizes the text is shown with line numbers but without
	// colours, so opening a huge entry stays instant.
	maxHighlightBytes = 100 << 10
	maxHighlightLines = 3000
)

// Lang is a detected language. The zero value is plain text.
type Lang struct {
	lexer chroma.Lexer
	name  string
}

// Name is the language's display name ("Python"), or "" for plain text.
func (l Lang) Name() string { return l.name }

// Known reports whether a language was detected.
func (l Lang) Known() bool { return l.lexer != nil }

// Badge is a short upper-case label for a type tag: "PYTHON", "JSON", "TEXT".
func (l Lang) Badge() string {
	if l.lexer == nil {
		return "TEXT"
	}
	b := strings.ToUpper(l.name)
	switch b {
	case "JAVASCRIPT":
		b = "JS"
	case "TYPESCRIPT":
		b = "TS"
	case "MARKDOWN":
		b = "MD"
	case "DOCKERFILE":
		b = "DOCKER"
	}
	if r := []rune(b); len(r) > 8 {
		b = string(r[:8])
	}
	return b
}

func lang(name string) Lang {
	l := lexers.Get(name)
	if l == nil {
		return Lang{}
	}
	return fromLexer(l)
}

func fromLexer(l chroma.Lexer) Lang {
	return Lang{lexer: chroma.Coalesce(l), name: displayName(l.Config().Name)}
}

// displayName capitalises names Chroma spells in lower case ("markdown").
func displayName(n string) string {
	if n != "" && n == strings.ToLower(n) {
		r := []rune(n)
		r[0] = unicode.ToUpper(r[0])
		return string(r)
	}
	return n
}

var (
	shellWords = map[string]bool{
		"ssh": true, "scp": true, "rsync": true, "git": true, "docker": true, "kubectl": true, "sudo": true,
		"curl": true, "wget": true, "cd": true, "ls": true, "cat": true, "grep": true, "find": true, "sed": true,
		"awk": true, "make": true, "go": true, "cargo": true, "npm": true, "yarn": true, "pip": true, "pip3": true,
		"python": true, "python3": true, "apt": true, "apt-get": true, "dnf": true, "pacman": true, "systemctl": true,
		"journalctl": true, "chmod": true, "chown": true, "mkdir": true, "rm": true, "cp": true, "mv": true,
		"tar": true, "echo": true, "export": true, "source": true, "brew": true, "tmux": true, "ip": true,
		"ping": true, "nc": true, "df": true, "du": true, "ps": true, "kill": true, "xargs": true, "helm": true,
		"terraform": true, "ansible": true, "gh": true, "ffmpeg": true, "yt-dlp": true, "nix": true, "flatpak": true,
	}
	reSQL      = regexp.MustCompile(`(?is)^\s*(select\s.+\sfrom\s|insert\s+into\s|update\s+\S+\s+set\s|delete\s+from\s|create\s+(table|index|view|database)\s|alter\s+table\s|with\s+\S+\s+as\s*\()`)
	reDiff     = regexp.MustCompile(`(?m)^(diff --git |--- [ab/]|\+\+\+ [ab/]|@@ -\d+)`)
	rePython   = regexp.MustCompile(`(?m)^\s*(def \w+\(.*\):|class \w+(\(.*\))?:|import \w+|from [\w.]+ import |if __name__ ?==|@\w+)`)
	reGo       = regexp.MustCompile(`(?m)^(package \w+|func (\(\w+ \*?\w+\) )?\w+\(|import \(|type \w+ (struct|interface))`)
	reRust     = regexp.MustCompile(`(?m)(^|\s)(fn \w+|let mut |use std::|impl(<.*>)? \w+|pub (fn|struct|enum)|#\[derive)`)
	reJS       = regexp.MustCompile(`(?m)(\b(const|let|var) \w+ ?=|=> ?[{(\w]|\bfunction\b ?\w*\(|console\.log\(|require\(['"]|export (default|const|function)|import .+ from ['"])`)
	reC        = regexp.MustCompile(`(?m)(^#include ?[<"]|\bint main ?\(|\bprintf\(|\bstd::|^using namespace )`)
	reJava     = regexp.MustCompile(`(?m)(public (static )?(class|void)|System\.out\.print|^import java\.)`)
	reHTML     = regexp.MustCompile(`(?i)(<!doctype html|<html[ >]|<(div|span|body|head|script|style|a|p|ul|li|table|form)[ >])`)
	reXML      = regexp.MustCompile(`^\s*<\?xml |^\s*<[A-Za-z][\w:.-]*[ >][\s\S]*</[A-Za-z][\w:.-]*>\s*$`)
	reYAMLKey  = regexp.MustCompile(`^\s*(- )?[\w.-]+:( .*)?$`)
	reYAMLItem = regexp.MustCompile(`^\s*- \S`)
	reTOMLHead = regexp.MustCompile(`(?m)^\[[\w.-]+\]\s*$`)
	reTOMLKV   = regexp.MustCompile(`(?m)^[\w.-]+ ?= ?\S`)
	reCSS      = regexp.MustCompile(`(?s)^\s*[.#@]?[\w\-:\s,>.#*\[\]="']+\{[^{}]*:[^{}]*;?[^{}]*\}`)
	reMD       = regexp.MustCompile("(?m)(^#{1,6} \\S|^```|^[-*] \\S.*\\n[-*] \\S|\\[[^\\]]+\\]\\([^)]+\\))")
	reDocker   = regexp.MustCompile(`(?m)^FROM \S+`)
)

// Detect picks a language from the file name (when there is one), a shebang,
// or the text's content. It errs on the side of plain text: prose, URLs and
// e-mail addresses are never coloured.
func Detect(filename, text string) Lang {
	if filename != "" {
		if l := lexers.Match(filename); l != nil && l.Config().Name != "plaintext" {
			return fromLexer(l)
		}
		if strings.EqualFold(baseName(filename), "Makefile") || strings.EqualFold(baseName(filename), "makefile") {
			return lang("make")
		}
	}
	t := strings.TrimSpace(text)
	if len(t) < 2 {
		return Lang{}
	}
	lines := strings.Count(t, "\n") + 1

	if strings.HasPrefix(t, "#!") { // shebang
		first := t
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			first = t[:i]
		}
		switch {
		case strings.Contains(first, "python"):
			return lang("python")
		case strings.Contains(first, "node"):
			return lang("javascript")
		case strings.Contains(first, "ruby"):
			return lang("ruby")
		case strings.Contains(first, "perl"):
			return lang("perl")
		case strings.Contains(first, "php"):
			return lang("php")
		}
		return lang("bash")
	}
	if (t[0] == '{' || t[0] == '[') && json.Valid([]byte(t)) && len(t) > 2 {
		return lang("json")
	}
	if reDiff.MatchString(t) && lines > 2 {
		return lang("diff")
	}
	if reDocker.MatchString(t) && lines >= 2 {
		return lang("docker")
	}
	if reSQL.MatchString(t) {
		return lang("sql")
	}
	if l := shellCommand(t); l.Known() {
		return l
	}
	if lines < 2 && len(t) < 40 {
		return Lang{} // a short single line is almost certainly not code
	}
	if strings.HasPrefix(t, "<?xml") || (reXML.MatchString(t) && !reHTML.MatchString(t)) {
		return lang("xml")
	}
	if reHTML.MatchString(t) && strings.Contains(t, "<") && strings.Contains(t, ">") {
		return lang("html")
	}
	switch {
	case reGo.MatchString(t) && (lines > 1):
		return lang("go")
	case rePython.MatchString(t) && lines > 1:
		return lang("python")
	case reRust.MatchString(t) && (strings.Contains(t, "{") || lines > 1):
		return lang("rust")
	case reJava.MatchString(t):
		return lang("java")
	case reC.MatchString(t):
		if strings.Contains(t, "std::") || strings.Contains(t, "using namespace") {
			return lang("cpp")
		}
		return lang("c")
	case reJS.MatchString(t):
		return lang("javascript")
	case reCSS.MatchString(t) && strings.Contains(t, ";"):
		return lang("css")
	case reTOMLHead.MatchString(t) && reTOMLKV.MatchString(t):
		return lang("toml")
	case looksLikeYAML(t):
		return lang("yaml")
	case reMD.MatchString(t) && lines > 1:
		return lang("markdown")
	}
	// Last resort: Chroma's own content analysers, trusted only for real multi-line code.
	if lines >= 4 {
		if l := lexers.Analyse(t); l != nil && l.Config().Name != "plaintext" {
			return fromLexer(l)
		}
	}
	return Lang{}
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// shellCommand recognises command lines ("ssh -i key host", "$ git status").
func shellCommand(t string) Lang {
	first := t
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		first = t[:i]
	}
	first = strings.TrimSpace(first)
	prompt := strings.HasPrefix(first, "$ ")
	first = strings.TrimPrefix(first, "$ ")
	word := first
	if i := strings.IndexAny(first, " \t"); i >= 0 {
		word = first[:i]
	}
	if word == "sudo" {
		if f := strings.Fields(first); len(f) > 1 {
			word = f[1]
		}
	}
	if strings.Contains(word, "=") && !strings.ContainsAny(word, " /") { // VAR=value cmd
		return lang("bash")
	}
	if prompt || (shellWords[word] && strings.Contains(first, " ")) {
		return lang("bash")
	}
	return Lang{}
}

func looksLikeYAML(t string) bool {
	n, keys, items := 0, 0, 0
	for _, ln := range strings.Split(t, "\n") {
		s := strings.TrimRight(ln, " \t")
		if s == "" || strings.HasPrefix(strings.TrimSpace(s), "#") {
			continue
		}
		n++
		switch {
		case reYAMLKey.MatchString(s):
			keys++
		case reYAMLItem.MatchString(s):
			items++
		}
	}
	return n >= 2 && keys >= 1 && (keys+items)*10 >= n*8 && !strings.ContainsAny(t, "{};")
}

// Options control Render.
type Options struct {
	Width       int  // total columns available, gutter included
	LineNumbers bool // show the bat-style gutter
	ASCII       bool // use | instead of │
	GutterStyle lipgloss.Style
}

// Result is the rendered text.
type Result struct {
	Lines     []string // each at most Width columns; ANSI styled
	LineCount int      // logical lines in the text
	Coloured  bool     // false when the text was too big to highlight
}

// Sanitize makes clipboard text safe to print: CRLF becomes LF, and control
// and invisible characters (ESC included) are replaced or dropped.
func Sanitize(text string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029':
			b.WriteRune('·')
		case unicode.Is(unicode.Cf, r) || r == '\ufeff':
			// zero-width and bidi controls: drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatter picks the terminal formatter matching the colours the terminal
// supports; ok is false when it has none (no colour output at all).
func formatter() (f chroma.Formatter, ok bool) {
	switch lipgloss.ColorProfile() {
	case termenv.TrueColor:
		return formatters.TTY16m, true
	case termenv.ANSI256:
		return formatters.TTY256, true
	case termenv.ANSI:
		return formatters.TTY16, true
	}
	return formatters.NoOp, false
}

func style() *chroma.Style {
	name := "tokyonight-night"
	if !lipgloss.HasDarkBackground() {
		name = "tokyonight-day"
	}
	s := styles.Get(name)
	if s == nil || s == styles.Fallback {
		s = styles.Fallback
	}
	return s
}

// Render highlights, numbers and wraps text.
func Render(text string, l Lang, o Options) Result {
	text = Sanitize(text)
	text = strings.TrimSuffix(text, "\n")
	logical := strings.Split(text, "\n")
	res := Result{LineCount: len(logical)}

	gutter := 0
	digits := max(minDigits, len(strconv.Itoa(len(logical)))) // like bat, numbers get a little room
	sep := " │ "
	if o.ASCII {
		sep = " | "
	}
	sepW := runewidth.StringWidth(sep) // columns, not bytes ("│" is 3 bytes but 1 column)
	if o.LineNumbers && o.Width-(digits+sepW) >= 8 {
		gutter = digits + sepW
	}
	codeW := max(1, o.Width-gutter)

	// tokens per logical line
	var toks [][]chroma.Token
	fmtr, hasColour := formatter()
	colour := l.lexer != nil && hasColour && len(text) <= maxHighlightBytes && len(logical) <= maxHighlightLines
	if colour {
		it, err := l.lexer.Tokenise(nil, text+"\n")
		if err == nil {
			toks = chroma.SplitTokensIntoLines(it.Tokens())
		} else {
			colour = false
		}
	}
	res.Coloured = colour
	if !colour {
		toks = make([][]chroma.Token, len(logical))
		for i, ln := range logical {
			toks[i] = []chroma.Token{{Type: chroma.Text, Value: ln}}
		}
	}

	st := style()
	var buf bytes.Buffer
	emit := func(num string, vis []chroma.Token) {
		buf.Reset()
		if gutter > 0 {
			buf.WriteString(o.GutterStyle.Render(num + sep))
		}
		if colour && len(vis) > 0 {
			var code bytes.Buffer
			_ = fmtr.Format(&code, st, chroma.Literator(vis...))
			buf.Write(code.Bytes())
		} else {
			for _, t := range vis {
				buf.WriteString(t.Value)
			}
		}
		res.Lines = append(res.Lines, buf.String())
	}

	blank := strings.Repeat(" ", digits)
	for i := 0; i < len(logical) && i < len(toks); i++ {
		num := leftPad(strconv.Itoa(i+1), digits)
		for _, vis := range wrapTokens(toks[i], codeW) {
			emit(num, vis)
			num = blank
		}
	}
	if len(res.Lines) == 0 {
		emit(leftPad("1", digits), nil)
	}
	return res
}

func leftPad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat(" ", n-len(s)) + s
}

// wrapTokens expands tabs and splits one logical line into visual lines of at
// most width columns, keeping each piece's token type (so colours continue on
// the wrapped part). The trailing newline of the line is removed.
func wrapTokens(line []chroma.Token, width int) [][]chroma.Token {
	var out [][]chroma.Token
	var cur []chroma.Token
	col := 0
	flush := func() {
		out = append(out, cur)
		cur, col = nil, 0
	}
	appendRune := func(tp chroma.TokenType, r rune) {
		if n := len(cur); n > 0 && cur[n-1].Type == tp {
			cur[n-1].Value += string(r)
		} else {
			cur = append(cur, chroma.Token{Type: tp, Value: string(r)})
		}
	}
	for _, t := range line {
		for _, r := range strings.TrimRight(t.Value, "\n") {
			switch {
			case r == '\t':
				n := tabWidth - col%tabWidth
				for k := 0; k < n; k++ {
					if col+1 > width {
						flush()
					}
					appendRune(t.Type, ' ')
					col++
				}
			case r == '\n' || r == '\r':
				continue
			default:
				w := runewidth.RuneWidth(r)
				if w == 0 && len(cur) > 0 { // combining mark joins the previous cell
					appendRune(t.Type, r)
					continue
				}
				if col+w > width && col > 0 {
					flush()
				}
				appendRune(t.Type, r)
				col += w
			}
		}
	}
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}
