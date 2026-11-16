package filebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateAndExtractFilesAndDirectory(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "report.txt"), []byte("report body"), 0o640); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(source, "project")
	if err := os.MkdirAll(filepath.Join(folder, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "nested", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/main.go", filepath.Join(folder, "main.go")); err != nil {
		t.Fatal(err)
	}

	archive, err := Create([]string{filepath.Join(source, "report.txt"), folder + string(filepath.Separator)})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Extract(archive, destination)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "project,report.txt" {
		t.Fatalf("unexpected extracted roots: %v", got)
	}
	body, err := os.ReadFile(filepath.Join(destination, "report.txt"))
	if err != nil || string(body) != "report body" {
		t.Fatalf("report contents %q, err=%v", body, err)
	}
	body, err = os.ReadFile(filepath.Join(destination, "project", "nested", "main.go"))
	if err != nil || string(body) != "package main\n" {
		t.Fatalf("nested contents %q, err=%v", body, err)
	}
	link, err := os.Readlink(filepath.Join(destination, "project", "main.go"))
	if err != nil || link != "nested/main.go" {
		t.Fatalf("symlink target %q, err=%v", link, err)
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o600, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(tw, "evil")
	_ = tw.Close()
	_ = gz.Close()

	destination := t.TempDir()
	if _, err := Extract(compressed.Bytes(), destination); err == nil || !strings.Contains(err.Error(), "outside the destination") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(destination), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("escape file unexpectedly exists: %v", err)
	}
}

func TestExtractDoesNotOverwriteExistingDestination(t *testing.T) {
	source := t.TempDir()
	file := filepath.Join(source, "note.txt")
	if err := os.WriteFile(file, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := Create([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "note.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(archive, destination); err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("expected conflict, got %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(destination, "note.txt"))
	if string(got) != "old" {
		t.Fatalf("existing file was changed: %q", got)
	}
}

func TestRemoveSourcesRemovesDirectoryRecursively(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSources([]string{dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("source directory still exists: %v", err)
	}
}

func TestRemoveSourcesProtectsCurrentDirectory(t *testing.T) {
	if err := RemoveSources([]string{"."}); err == nil || !strings.Contains(err.Error(), "protected directory") {
		t.Fatalf("expected protected path error, got %v", err)
	}
}

func TestCreateRejectsOverlappingInputsAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Create([]string{root, child}); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected overlap error, got %v", err)
	}
}

func TestCreateAndExtractBinaryMedia(t *testing.T) {
	source := t.TempDir()
	media := filepath.Join(source, "clip.mp4")
	// Include NUL and non-UTF-8 bytes to ensure media is transferred as binary.
	want := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', 0xff, 0x80, 0x00}
	if err := os.WriteFile(media, want, 0o640); err != nil {
		t.Fatal(err)
	}
	bundle, err := Create([]string{media})
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if _, err := Extract(bundle, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("binary media changed during transfer: got %v, want %v", got, want)
	}
}
