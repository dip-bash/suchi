package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/dip-bash/suchi/internal/textutil"
)

// RenderText formats clipboard text with bat when it is installed. The
// filename hint lets bat select a syntax while stdin keeps the preview
// independent of temporary files. A plain wrapped preview remains available
// on systems without bat (or when bat cannot render the input).
func RenderText(text string, width int) []string {
	width = max(width, 1)
	// Do not make a huge clipboard entry spend seconds in an external
	// highlighter. The plain renderer is fast and keeps the popup responsive.
	if len(text) > 2<<20 {
		return textutil.Wrap(text, width)
	}
	bat, err := exec.LookPath("bat")
	if err != nil {
		bat, err = exec.LookPath("batcat") // Debian/Ubuntu package name
	}
	if err == nil && text != "" {
		name := syntaxFilename(text)
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, bat,
			"--color=always",
			"--style=numbers",
			"--paging=never",
			"--wrap=character",
			"--terminal-width", itoa(width),
			"--file-name", name,
		)
		cmd.WaitDelay = 250 * time.Millisecond
		cmd.Stdin = strings.NewReader(text)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
			if len(lines) > 0 && lines[0] != "" {
				return lines
			}
		}
	}
	return textutil.Wrap(text, width)
}

func syntaxFilename(text string) string {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(trimmed, "#!"):
		if strings.Contains(lower, "python") {
			return "clipboard.py"
		}
		if strings.Contains(lower, "node") || strings.Contains(lower, "deno") {
			return "clipboard.js"
		}
		return "clipboard.sh"
	case json.Valid([]byte(trimmed)):
		return "clipboard.json"
	case strings.HasPrefix(trimmed, "<") && strings.Contains(lower, ">"):
		return "clipboard.html"
	case strings.Contains(trimmed, "package ") && (strings.Contains(trimmed, "func ") || strings.Contains(trimmed, "import ")):
		return "clipboard.go"
	case strings.Contains(trimmed, "def ") && strings.Contains(trimmed, ":"):
		return "clipboard.py"
	case strings.Contains(trimmed, "#include ") || strings.Contains(trimmed, "int main("):
		return "clipboard.c"
	case strings.Contains(trimmed, "fn main(") || strings.Contains(trimmed, "use std::"):
		return "clipboard.rs"
	case strings.Contains(trimmed, "const ") && (strings.Contains(trimmed, "=>") || strings.Contains(trimmed, "function ")):
		return "clipboard.js"
	case strings.Contains(trimmed, "SELECT ") || strings.Contains(trimmed, "select ") && strings.Contains(lower, " from "):
		return "clipboard.sql"
	default:
		return "clipboard.txt"
	}
}

func itoa(n int) string {
	// Avoid pulling formatting machinery into this small rendering helper.
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
