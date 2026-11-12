package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dip-bash/suchi/internal/store"
)

// sandbox points suchi at a scratch database and state dir and hides any
// real display server.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "suchi.db")
	t.Setenv("SUCHI_DB", db)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("SUCHI_BACKEND", "auto")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	return db
}

func run(t *testing.T, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = Run(args, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func clips(t *testing.T, db string) []string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, _ := st.ListClips(context.Background())
	var out []string
	for _, c := range list {
		out = append(out, c.Preview)
	}
	return out
}

func TestAddFromArgsAndStdin(t *testing.T) {
	db := sandbox(t)
	if code, _, e := run(t, "", "add", "from", "args"); code != 0 {
		t.Fatalf("code=%d %s", code, e)
	}
	if code, _, e := run(t, "from stdin\n", "add"); code != 0 {
		t.Fatalf("code=%d %s", code, e)
	}
	got := clips(t, db)
	if len(got) != 2 || got[0] != "from stdin" || got[1] != "from args" { // stdin's trailing newline is dropped
		t.Fatalf("got %q", got)
	}
	if code, _, e := run(t, "  \n", "add"); code != 1 || !strings.Contains(e, "nothing added") {
		t.Fatalf("blank add: code=%d %q", code, e)
	}
}

func TestGetSnippet(t *testing.T) {
	db := sandbox(t)
	st, _ := store.Open(db)
	_, _ = st.AddSnippet(context.Background(), store.Snippet{Name: "mail", Body: "me@example.com"})
	_, _ = st.AddSnippet(context.Background(), store.Snippet{Name: "Mail", Body: "work@example.com"})
	_ = st.Close()

	code, out, e := run(t, "", "get", "MAIL")
	if code != 0 || out != "me@example.com" || !strings.Contains(e, "2 snippets") {
		t.Fatalf("code=%d out=%q err=%q", code, out, e)
	}
	if code, _, e := run(t, "", "get", "nope"); code != 1 || !strings.Contains(e, "no snippet named") {
		t.Fatalf("code=%d %q", code, e)
	}
	if code, _, _ := run(t, "", "get"); code != 2 {
		t.Fatalf("missing name must be a usage error, got %d", code)
	}
}

func TestClear(t *testing.T) {
	db := sandbox(t)
	_, _, _ = run(t, "", "add", "one")
	_, _, _ = run(t, "", "add", "two")
	if code, _, e := run(t, "n\n", "clear"); code != 0 || !strings.Contains(e, "cancelled") || len(clips(t, db)) != 2 {
		t.Fatalf("declined clear must keep everything: code=%d %q", code, e)
	}
	if code, out, _ := run(t, "", "clear", "-y"); code != 0 || !strings.Contains(out, "deleted 2") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if len(clips(t, db)) != 0 {
		t.Fatal("history must be empty")
	}
}

func TestImport(t *testing.T) {
	db := sandbox(t)
	p := filepath.Join(t.TempDir(), "history.json")
	_ = os.WriteFile(p, []byte(`[{"text":"legacy","pinned":false,"copiedAt":1700000000000,"usedAt":0}]`), 0o600)
	if code, out, e := run(t, "", "import", p); code != 0 || !strings.Contains(out, "imported 1") {
		t.Fatalf("code=%d out=%q err=%q", code, out, e)
	}
	if got := clips(t, db); len(got) != 1 || got[0] != "legacy" {
		t.Fatalf("got %q", got)
	}
	if code, _, _ := run(t, "", "import", filepath.Join(t.TempDir(), "missing.json")); code != 1 {
		t.Fatal("missing file must fail")
	}
}

func TestUsageAndErrors(t *testing.T) {
	sandbox(t)
	if code, out, _ := run(t, "", "--help"); code != 0 || !strings.Contains(out, "Usage:") ||
		!strings.Contains(out, "--allow-multiple") || !strings.Contains(out, "one suchi interface") {
		t.Fatalf("help: code=%d", code)
	}
	if code, out, _ := run(t, "", "version"); code != 0 || !strings.HasPrefix(out, "suchi ") {
		t.Fatalf("version: code=%d out=%q", code, out)
	}
	if code, _, e := run(t, "", "bogus"); code != 2 || !strings.Contains(e, "unknown command") {
		t.Fatalf("unknown command: code=%d", code)
	}
	if code, _, _ := run(t, "", "add", "--no-such-flag"); code != 2 {
		t.Fatalf("bad flag must be a usage error, got %d", code)
	}
	if code, _, e := run(t, "", "add", "--selection", "nope", "x"); code != 1 || !strings.Contains(e, "selection must be") {
		t.Fatalf("bad selection: code=%d %q", code, e)
	}
}

func TestDaemonRefusesWithoutDisplayServer(t *testing.T) {
	sandbox(t)
	code, _, e := run(t, "", "daemon")
	if code != 1 || !strings.Contains(e, "no display server") || !strings.Contains(e, "suchi add") {
		t.Fatalf("code=%d err=%q", code, e)
	}
}

func TestStatusAndStopWhenNotRunning(t *testing.T) {
	sandbox(t)
	if code, out, _ := run(t, "", "status"); code != 0 || !strings.Contains(out, "not running") || !strings.Contains(out, "backend") {
		t.Fatalf("status: code=%d out=%q", code, out)
	}
	if code, out, _ := run(t, "", "stop"); code != 0 || !strings.Contains(out, "not running") {
		t.Fatalf("stop: code=%d out=%q", code, out)
	}
}
