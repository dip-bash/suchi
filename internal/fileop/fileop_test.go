package fileop

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPasteCopiesFilesAndFolders(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	dstDir := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(srcDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(srcDir, "a.txt"), "a")
	write(t, filepath.Join(srcDir, "nested", "b.txt"), "b")
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Paste([]string{filepath.Join(srcDir, "a.txt"), srcDir}, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pasted %v", got)
	}
	if b, err := os.ReadFile(filepath.Join(dstDir, "a.txt")); err != nil || string(b) != "a" {
		t.Fatalf("a.txt: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dstDir, "src", "nested", "b.txt")); err != nil || string(b) != "b" {
		t.Fatalf("nested/b.txt: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "a.txt")); err != nil {
		t.Fatal("copy must leave the source in place")
	}
}

func TestPasteCutMovesSources(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "cut.txt")
	dstDir := filepath.Join(root, "dst")
	write(t, src, "cut")
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Paste([]string{src}, dstDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != filepath.Join(dstDir, "cut.txt") {
		t.Fatalf("got %v", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("cut must remove the source")
	}
	if b, err := os.ReadFile(got[0]); err != nil || string(b) != "cut" {
		t.Fatalf("pasted: %q %v", b, err)
	}
}

func TestPastePreservesSymlinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "link")
	if err := os.Symlink("target", src); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	dstDir := filepath.Join(root, "dst")
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Paste([]string{src}, dstDir, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(dstDir, "link"))
	if err != nil || got != "target" {
		t.Fatalf("link = %q, %v", got, err)
	}
}

func TestPasteRefusesDirectoryIntoItself(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "dir")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Paste([]string{src}, src, false); err == nil {
		t.Fatal("pasting a folder into itself must fail")
	}
	if _, err := Paste([]string{src}, filepath.Join(src, "sub"), false); err == nil {
		t.Fatal("pasting a folder into its own subfolder must fail")
	}
	// A symlink to the source folder must be caught too.
	link := filepath.Join(root, "link")
	if err := os.Symlink(src, link); err == nil {
		if _, err := Paste([]string{src}, link, false); err == nil {
			t.Fatal("pasting a folder through a symlink into itself must fail")
		}
	}
}

func TestPasteRefusesConflictsAndSameDirectory(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "a.txt")
	write(t, src, "a")
	if _, err := Paste([]string{src}, root, false); err == nil {
		t.Fatal("pasting into the source directory must fail")
	}

	dstDir := filepath.Join(root, "dst")
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dstDir, "a.txt"), "existing")
	if _, err := Paste([]string{src}, dstDir, false); err == nil {
		t.Fatal("pasting over an existing file must fail")
	}
}
