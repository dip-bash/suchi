package highlight

import (
	"strings"
	"testing"
	"time"

	"github.com/muesli/termenv"
)

// Opening a big entry with "e" must stay quick.
func TestRenderPerformanceOnLargeText(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	code := strings.Repeat("def handler(request):\n    value = request.get('x', 42)  # comment\n    return {'ok': True, 'v': value}\n\n", 700) // ~68 KB, 2800 lines: just under the colouring cap
	start := time.Now()
	lang := Detect("", code)
	r := Render(code, lang, Options{Width: 100, LineNumbers: true})
	took := time.Since(start)
	t.Logf("%d KB, %d lines, coloured=%v: detect+render %v", len(code)/1024, r.LineCount, r.Coloured, took)
	if took > 3*time.Second*slowdown {
		t.Fatalf("too slow: %v", took)
	}
	big := strings.Repeat("x = 1\n", 400000) // 2.4 MB: numbered, not highlighted
	start = time.Now()
	r = Render(big, lang, Options{Width: 100, LineNumbers: true})
	t.Logf("%d KB uncoloured: %v", len(big)/1024, time.Since(start))
	if time.Since(start) > 3*time.Second*slowdown {
		t.Fatalf("uncoloured path too slow: %v", time.Since(start))
	}
}
