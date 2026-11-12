package preview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Category groups file types for the coloured type badge.
type Category int

// File categories.
const (
	CatOther Category = iota
	CatImage
	CatVideo
	CatAudio
	CatDoc
	CatArchive
	CatCode
	CatText
	CatFolder
)

var extCategory = func() map[string]Category {
	m := map[string]Category{}
	add := func(c Category, exts string) {
		for _, e := range strings.Fields(exts) {
			m[e] = c
		}
	}
	add(CatImage, "png jpg jpeg gif webp bmp tif tiff svg ico avif heic heif raw psd xcf")
	add(CatVideo, "mp4 mkv webm mov avi flv wmv m4v mpg mpeg ts 3gp ogv")
	add(CatAudio, "mp3 flac wav ogg opus m4a aac wma aiff mid midi")
	add(CatDoc, "pdf doc docx odt rtf xls xlsx ods ppt pptx odp epub mobi djvu")
	add(CatArchive, "zip tar gz tgz bz2 xz 7z rar zst lz4 deb rpm iso dmg apk")
	add(CatCode, "go py js jsx tsx rs c h cc cpp hpp java kt rb php sh bash zsh fish lua pl sql json yaml yml toml xml html htm css scss vue svelte")
	add(CatText, "txt md rst log csv tsv ini conf cfg")
	return m
}()

// Info describes a copied file for the details card.
type Info struct {
	Path     string
	Name     string
	Label    string // short type label for the badge: "PNG", "MP4", "DIR", "FILE"
	Category Category
	Size     int64
	ModTime  time.Time
	IsDir    bool
	Exists   bool
}

// Classify returns the badge label and category for a path.
func Classify(path string, isDir bool) (label string, cat Category) {
	if isDir {
		return "DIR", CatFolder
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return "FILE", CatOther
	}
	if len(ext) > 5 {
		ext = ext[:5]
	}
	return strings.ToUpper(ext), extCategory[ext]
}

// Stat gathers the details of a path; missing files are reported, not errors.
func Stat(path string) Info {
	in := Info{Path: path, Name: filepath.Base(path)}
	if st, err := os.Stat(path); err == nil {
		in.Exists, in.IsDir, in.Size, in.ModTime = true, st.IsDir(), st.Size(), st.ModTime()
	}
	in.Label, in.Category = Classify(path, in.IsDir)
	return in
}

// LabelForMime returns the badge label for an image MIME type ("image/jpeg" -> "JPG").
func LabelForMime(mime string) string {
	m := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch m {
	case "image/jpeg", "image/jpg":
		return "JPG"
	case "image/x-ms-bmp":
		return "BMP"
	case "image/svg+xml":
		return "SVG"
	}
	if i := strings.IndexByte(m, '/'); i >= 0 && i+1 < len(m) {
		return strings.ToUpper(strings.TrimPrefix(m[i+1:], "x-"))
	}
	return "IMG"
}

// DecodableImage reports whether suchi can draw this file extension.
func DecodableImage(path string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "png", "jpg", "jpeg", "gif", "webp", "bmp", "tif", "tiff":
		return true
	}
	return false
}

// HumanSize formats a byte count: 512 B, 1.5 KB, 3.2 MB.
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// ShortPath abbreviates the home directory and, when the path is too wide,
// keeps its end (the part you recognise): "~/Videos/demo.mp4" or ".../a/demo.mp4".
func ShortPath(path string, width int) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && (path == h || strings.HasPrefix(path, h+"/")) {
		path = "~" + strings.TrimPrefix(path, h)
	}
	r := []rune(path)
	if width < 4 || len(r) <= width {
		return path
	}
	// cut from the left at a path separator when possible
	tail := string(r[len(r)-(width-3):])
	if i := strings.IndexByte(tail, '/'); i >= 0 && i < len(tail)-1 {
		tail = tail[i:]
	}
	return "..." + tail
}
