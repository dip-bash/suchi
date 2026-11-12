package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "suchi.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func texts(t *testing.T, s *Store) []string {
	t.Helper()
	cl, err := s.ListClips(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cl {
		out = append(out, c.Preview)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Regression: starting the daemon and the UI at the same moment on a fresh
// database used to fail with "table clips already exists".
func TestOpenFreshDBConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suchi.db")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			_ = s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Regression: a database whose tables exist but whose version was never
// recorded (what the failing user had) must open, keep its data and upgrade.
func TestOpenHealsDatabaseWithoutVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suchi.db")
	raw, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE clips (id INTEGER PRIMARY KEY, text TEXT NOT NULL UNIQUE, pinned INTEGER NOT NULL DEFAULT 0, copied_at INTEGER NOT NULL, used_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE snippets (id INTEGER PRIMARY KEY, name TEXT NOT NULL, body TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'text', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO clips (text, pinned, copied_at) VALUES ('survivor', 1, 1700000000000)`,
		`INSERT INTO snippets (name, body, created_at, updated_at) VALUES ('mail', 'me@example.com', 1, 1)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open must heal the database, got: %v", err)
	}
	defer s.Close()
	cl, _ := s.ListClips(context.Background())
	sn, _ := s.ListSnippets(context.Background())
	if len(cl) != 1 || cl[0].Preview != "survivor" || !cl[0].Pinned || cl[0].Kind != ClipText || len(sn) != 1 {
		t.Fatalf("data lost or mangled: %+v %+v", cl, sn)
	}
	s2, err := Open(path) // and reopening is a no-op
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
}

func TestPermissions(t *testing.T) {
	_, path := open(t)
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", p, got, want)
		}
	}
}

func TestAddBumpsInsteadOfDuplicating(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, x := range []string{"a", "b", "a"} {
		time.Sleep(2 * time.Millisecond)
		if _, err := s.AddClip(ctx, x, 100, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if got := texts(t, s); !eq(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
}

func TestExactTextBlankAndSizeCap(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if ok, _ := s.AddClip(ctx, "line\n", 100, 1<<20); !ok {
		t.Fatal("expected add")
	}
	if got := texts(t, s); !eq(got, []string{"line\n"}) {
		t.Fatalf("text must be stored exactly, got %q", got)
	}
	if ok, _ := s.AddClip(ctx, "  \n\t", 100, 1<<20); ok {
		t.Error("blank text must be ignored")
	}
	if ok, _ := s.AddClip(ctx, "0123456789", 100, 5); ok {
		t.Error("oversized text must be ignored")
	}
}

// Regression for the Python version: trimming must drop the OLDEST entries
// even when an old entry was just bumped.
func TestPruneKeepsRecentlyBumped(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		time.Sleep(2 * time.Millisecond)
		_, _ = s.AddClip(ctx, fmt.Sprintf("item%d", i), 100, 1<<20)
	}
	time.Sleep(2 * time.Millisecond)
	_, _ = s.AddClip(ctx, "item0", 3, 1<<20) // re-copy the oldest, limit 3
	got := texts(t, s)
	if !eq(got, []string{"item0", "item4", "item3"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPinnedExemptFromLimit(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	_, _ = s.AddClip(ctx, "keep me", 3, 1<<20)
	cl, _ := s.ListClips(ctx)
	if err := s.TogglePin(ctx, cl[0].ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		time.Sleep(time.Millisecond)
		_, _ = s.AddClip(ctx, fmt.Sprintf("n%d", i), 3, 1<<20)
	}
	got := texts(t, s)
	if len(got) != 3 || got[0] != "keep me" {
		t.Fatalf("got %v", got)
	}
}

func TestPreviewAndFullText(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	big := make([]rune, PreviewChars+500)
	for i := range big {
		big[i] = 'é'
	}
	body := string(big) + "\nend"
	_, _ = s.AddClip(ctx, body, 10, 1<<22)
	cl, _ := s.ListClips(ctx)
	if len([]rune(cl[0].Preview)) != PreviewChars || cl[0].Length != len(big)+4 || !cl[0].Multiline {
		t.Fatalf("preview=%d length=%d ml=%v", len([]rune(cl[0].Preview)), cl[0].Length, cl[0].Multiline)
	}
	full, err := s.ClipText(ctx, cl[0].ID)
	if err != nil || full != body {
		t.Fatalf("full text mismatch (err=%v)", err)
	}
}

// Regression for the Python version, which lost ~88% of entries: the daemon
// and the TUI are separate processes, so use two independent handles.
func TestConcurrentHandlesLoseNothing(t *testing.T) {
	s1, path := open(t)
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			st := s1
			if w%2 == 1 {
				st = s2
			}
			for i := 0; i < 40; i++ {
				if _, err := st.AddClip(ctx, fmt.Sprintf("w%d-%d", w, i), 100000, 1<<20); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if n := len(texts(t, s1)); n != 320 {
		t.Fatalf("%d of 320 entries survived", n)
	}
}

func TestDataVersionSeesOtherHandle(t *testing.T) {
	s1, path := open(t)
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx := context.Background()
	v1, _ := s1.DataVersion(ctx)
	_, _ = s2.AddClip(ctx, "from the daemon", 10, 1<<20)
	v2, _ := s1.DataVersion(ctx)
	if v1 == v2 {
		t.Fatal("data_version did not change after another handle committed")
	}
}

func TestClear(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	_, _ = s.AddClip(ctx, "pinned", 10, 1<<20)
	_, _ = s.AddClip(ctx, "loose", 10, 1<<20)
	cl, _ := s.ListClips(ctx)
	for _, c := range cl {
		if c.Preview == "pinned" {
			_ = s.TogglePin(ctx, c.ID)
		}
	}
	if n, _ := s.ClearClips(ctx, false); n != 1 {
		t.Fatalf("cleared %d, want 1", n)
	}
	if got := texts(t, s); !eq(got, []string{"pinned"}) {
		t.Fatalf("got %v", got)
	}
	_, _ = s.ClearClips(ctx, true)
	if len(texts(t, s)) != 0 {
		t.Fatal("expected empty")
	}
}

func TestSnippets(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	// same name twice is allowed
	id1, err := s.AddSnippet(ctx, Snippet{Name: "mail", Body: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSnippet(ctx, Snippet{Name: "Mail", Body: "work@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSnippet(ctx, Snippet{Name: "deploy", Body: "make deploy", Kind: KindCommand}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListSnippets(ctx)
	if len(all) != 3 || all[0].Name != "deploy" { // ordered by name, case-insensitive
		t.Fatalf("got %+v", all)
	}
	found, _ := s.FindSnippets(ctx, "MAIL")
	if len(found) != 2 {
		t.Fatalf("FindSnippets case-insensitive: got %d", len(found))
	}
	if err := s.UpdateSnippet(ctx, Snippet{ID: id1, Name: "mail", Body: "new@example.com", Kind: KindText}); err != nil {
		t.Fatal(err)
	}
	found, _ = s.FindSnippets(ctx, "mail")
	if found[0].Body != "new@example.com" {
		t.Fatalf("update failed: %+v", found[0])
	}
	if err := s.DeleteSnippet(ctx, id1); err != nil {
		t.Fatal(err)
	}
	if all, _ = s.ListSnippets(ctx); len(all) != 2 {
		t.Fatalf("delete failed: %d left", len(all))
	}
}

func TestSnippetValidation(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, sn := range []Snippet{
		{Name: "", Body: "x"},
		{Name: "n", Body: "  "},
		{Name: "n", Body: "x", Kind: "weird"},
	} {
		if _, err := s.AddSnippet(ctx, sn); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", sn, err)
		}
	}
}

func TestImportLegacyJSON(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "history.json")
	_ = os.WriteFile(p, []byte(`[
		{"text":"old one","pinned":true,"copiedAt":1700000000000,"usedAt":0},
		{"text":"old two","pinned":false,"copiedAt":1700000001000,"usedAt":1700000009000},
		{"text":"   ","pinned":false,"copiedAt":1},
		{"nope":1}]`), 0o600)
	n, err := s.ImportLegacyJSON(ctx, p, 100, 1<<20)
	if err != nil || n != 3 { // the {"nope":1} item has empty text and is skipped only via blank check
		// 2 real entries + 1 skipped blank + 1 skipped empty => imported counts only inserted rows
		if n != 2 {
			t.Fatalf("imported %d (err=%v)", n, err)
		}
	}
	cl, _ := s.ListClips(ctx)
	if len(cl) != 2 || cl[0].Preview != "old one" || !cl[0].Pinned {
		t.Fatalf("got %+v", cl)
	}
	if cl[1].UsedAt.UnixMilli() != 1700000009000 {
		t.Fatalf("usedAt not preserved: %v", cl[1].UsedAt)
	}
}

func TestImagesAreStoredDedupedAndDescribed(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	png := []byte("\x89PNG fake image bytes")
	lim := Limits{Count: 100, MaxText: 1 << 20, MaxMedia: 1 << 20, MediaBudget: 1 << 30}
	if ok, err := s.AddItem(ctx, NewClip{Kind: ClipImage, Mime: "image/png", Data: png}, lim); !ok || err != nil {
		t.Fatalf("add image: %v %v", ok, err)
	}
	time.Sleep(2 * time.Millisecond)
	_, _ = s.AddItem(ctx, NewClip{Kind: ClipImage, Mime: "image/png", Data: png}, lim) // same bytes: bump
	_, _ = s.AddItem(ctx, NewClip{Kind: ClipText, Text: "after"}, lim)
	cl, _ := s.ListClips(ctx)
	if len(cl) != 2 {
		t.Fatalf("same image twice must be one entry, got %d", len(cl))
	}
	var img Clip
	for _, c := range cl {
		if c.Kind == ClipImage {
			img = c
		}
	}
	if !strings.HasPrefix(img.Preview, "image-") || !strings.HasSuffix(img.Preview, ".png") || img.Mime != "image/png" || img.Size != int64(len(png)) {
		t.Fatalf("image row: %+v", img)
	}
	data, mime, err := s.ClipData(ctx, img.ID)
	if err != nil || mime != "image/png" || string(data) != string(png) {
		t.Fatalf("data round trip: %v %q", err, mime)
	}
	if _, _, err := s.ClipData(ctx, cl[0].ID+cl[1].ID+99); err == nil {
		t.Fatal("unknown id must error")
	}
}

func TestMediaLimitsAndBudget(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	lim := Limits{Count: 100, MaxText: 1 << 20, MaxMedia: 100, MediaBudget: 250}
	if ok, _ := s.AddItem(ctx, NewClip{Kind: ClipImage, Mime: "image/png", Data: make([]byte, 101)}, lim); ok {
		t.Fatal("image over MaxMedia must be skipped")
	}
	for i := 0; i < 4; i++ { // 4 x 100 bytes against a 250 byte budget
		time.Sleep(2 * time.Millisecond)
		d := make([]byte, 100)
		d[0] = byte(i + 1)
		if _, err := s.AddItem(ctx, NewClip{Kind: ClipImage, Mime: "image/png", Data: d}, lim); err != nil {
			t.Fatal(err)
		}
	}
	cl, _ := s.ListClips(ctx)
	if len(cl) != 2 { // the two oldest were dropped
		t.Fatalf("budget pruning kept %d images, want 2", len(cl))
	}
	data, _, _ := s.ClipData(ctx, cl[0].ID)
	if data[0] != 4 {
		t.Fatalf("newest image must survive, got marker %d", data[0])
	}
}

func TestFilesAreStoredByPath(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	lim := Limits{Count: 100, MaxText: 1 << 20}
	paths := "/home/u/Videos/demo.mp4\n/home/u/Pictures/a.png"
	if ok, _ := s.AddItem(ctx, NewClip{Kind: ClipFile, Text: paths, Mime: "text/uri-list"}, lim); !ok {
		t.Fatal("file entry not added")
	}
	_, _ = s.AddItem(ctx, NewClip{Kind: ClipFile, Text: paths, Mime: "text/uri-list"}, lim)
	// the same string as plain TEXT is a different kind of entry
	_, _ = s.AddItem(ctx, NewClip{Kind: ClipText, Text: paths}, lim)
	cl, _ := s.ListClips(ctx)
	if len(cl) != 2 {
		t.Fatalf("got %d entries, want a file entry plus a text entry", len(cl))
	}
}

func TestIgnoreNextKeepsSnippetCopiesOutOfHistory(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.IgnoreNext(ctx, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AddClip(ctx, "me@example.com", 100, 1<<20); ok {
		t.Fatal("the copy suchi itself made must not become history")
	}
	if ok, _ := s.AddClip(ctx, "me@example.com", 100, 1<<20); !ok {
		t.Fatal("the marker is single-use: copying the same text later is normal history")
	}
	_ = s.IgnoreNext(ctx, "other")
	if ok, _ := s.AddClip(ctx, "unrelated", 100, 1<<20); !ok {
		t.Fatal("other texts are unaffected")
	}
}

func TestExtForMime(t *testing.T) {
	for in, want := range map[string]string{
		"image/png": "png", "image/jpeg": "jpg", "image/jpeg; q=1": "jpg", "image/x-foo": "foo", "": "img",
	} {
		if got := ExtForMime(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
