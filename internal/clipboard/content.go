package clipboard

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"strings"
)

// Kind is what an Item holds.
type Kind string

// Item kinds.
const (
	KindText  Kind = "text"
	KindImage Kind = "image"
	KindFiles Kind = "files"
)

// Item is one thing on the clipboard: text, an image, or a list of copied files.
type Item struct {
	Kind Kind
	Text string // the text, or the absolute file paths (one per line) for KindFiles
	MIME string // image/png ... for images; text/uri-list for files
	Data []byte // image bytes
}

// Limits bound what Watch reports.
type Limits struct {
	MaxText  int // skip text larger than this many bytes
	MaxMedia int // skip images larger than this many bytes
}

// Paths returns the file paths of a KindFiles item.
func (it Item) Paths() []string {
	if it.Kind != KindFiles {
		return nil
	}
	return strings.Split(it.Text, "\n")
}

// Plain is the closest plain-text form of the item (used when only text can be delivered).
func (it Item) Plain() string {
	if it.Kind == KindImage {
		return ""
	}
	return it.Text
}

// Format is one clipboard representation offered to other applications.
type Format struct {
	MIME string
	Data []byte
}

const (
	mimeURIList   = "text/uri-list"
	mimeGnomeCopy = "x-special/gnome-copied-files"
)

var imageMimes = []string{"image/png", "image/jpeg", "image/webp", "image/gif", "image/bmp", "image/x-ms-bmp", "image/tiff"}

// candidate is a (kind, mime) pair worth trying to read, best first.
type candidate struct {
	kind Kind
	mime string
}

func has(list []string, s string) bool {
	for _, m := range list {
		if m == s {
			return true
		}
	}
	return false
}

// candidates ranks what to read from an offer: copied files first, then
// text, then images (a spreadsheet offers text and a picture of itself; the
// text is what you meant). It returns nil for entries flagged secret.
func candidates(mimes []string) []candidate {
	for _, s := range secretMimes {
		if has(mimes, s) {
			return nil
		}
	}
	var out []candidate
	if has(mimes, mimeURIList) {
		out = append(out, candidate{KindFiles, mimeURIList})
	} else if has(mimes, mimeGnomeCopy) {
		out = append(out, candidate{KindFiles, mimeGnomeCopy})
	}
	text := ""
	for _, want := range textMimes {
		if has(mimes, want) {
			text = want
			break
		}
	}
	if text == "" {
		for _, m := range mimes {
			if strings.HasPrefix(m, "text/plain") {
				text = m
				break
			}
		}
	}
	if text != "" {
		out = append(out, candidate{KindText, text})
	}
	img := ""
	for _, want := range imageMimes {
		if has(mimes, want) {
			img = want
			break
		}
	}
	if img == "" {
		for _, m := range mimes {
			if strings.HasPrefix(m, "image/") && m != "image/svg+xml" {
				img = m
				break
			}
		}
	}
	if img != "" {
		out = append(out, candidate{KindImage, img})
	}
	return out
}

// buildItem turns raw offer data into an Item. ok is false when the data is
// not usable (for example a uri-list of web links, which is then read as text).
func buildItem(c candidate, data []byte, lim Limits) (Item, bool) {
	switch c.kind {
	case KindFiles:
		paths := ParseFileURIs(string(data))
		if len(paths) == 0 {
			return Item{}, false
		}
		return Item{Kind: KindFiles, Text: strings.Join(paths, "\n"), MIME: mimeURIList}, true
	case KindImage:
		if len(data) == 0 || (lim.MaxMedia > 0 && len(data) > lim.MaxMedia) {
			return Item{}, false
		}
		return Item{Kind: KindImage, MIME: c.mime, Data: data}, true
	default:
		if lim.MaxText > 0 && len(data) > lim.MaxText {
			return Item{}, false
		}
		return Item{Kind: KindText, Text: strings.ToValidUTF8(string(data), "\uFFFD")}, true
	}
}

// maxFor is how many bytes may be read for a candidate.
func maxFor(c candidate, lim Limits) int {
	n := lim.MaxText
	if c.kind == KindImage {
		n = lim.MaxMedia
	}
	if c.kind == KindFiles || n <= 0 {
		n = 1 << 20
	}
	return n
}

// ParseFileURIs extracts local file paths from a text/uri-list (or the
// x-special/gnome-copied-files variant, whose first line is "copy" or "cut").
// Web links and unparsable lines are ignored.
func ParseFileURIs(s string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || line == "copy" || line == "cut" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || u.Path == "" {
			continue
		}
		out = append(out, filepath.Clean(u.Path))
	}
	return out
}

// FileURI is the inverse of ParseFileURIs for one path.
func FileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// Formats returns every representation worth offering for an item.
func Formats(it Item) []Format {
	switch it.Kind {
	case KindImage:
		return []Format{{it.MIME, it.Data}}
	case KindFiles:
		paths := it.Paths()
		uris := make([]string, len(paths))
		for i, p := range paths {
			uris[i] = FileURI(p)
		}
		return []Format{
			{mimeURIList, []byte(strings.Join(uris, "\r\n") + "\r\n")},
			{mimeGnomeCopy, []byte("copy\n" + strings.Join(uris, "\n"))},
			{"text/plain;charset=utf-8", []byte(it.Text)},
			{"text/plain", []byte(it.Text)},
		}
	default:
		fs := make([]Format, len(textMimes))
		for i, m := range textMimes {
			fs[i] = Format{m, []byte(it.Text)}
		}
		return fs
	}
}

// encodeItem / decodeItem frame an Item for the detached helper process:
// one JSON header line, then the raw body.
type itemHeader struct {
	Kind Kind   `json:"kind"`
	MIME string `json:"mime"`
}

func encodeItem(w io.Writer, it Item) error {
	h, _ := json.Marshal(itemHeader{it.Kind, it.MIME})
	if _, err := w.Write(append(h, '\n')); err != nil {
		return err
	}
	body := []byte(it.Text)
	if it.Kind == KindImage {
		body = it.Data
	}
	_, err := w.Write(body)
	return err
}

func decodeItem(r io.Reader) (Item, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return Item{}, err
	}
	var h itemHeader
	if err := json.Unmarshal(line, &h); err != nil {
		return Item{}, err
	}
	body, err := io.ReadAll(br)
	if err != nil {
		return Item{}, err
	}
	switch h.Kind {
	case KindImage:
		return Item{Kind: KindImage, MIME: h.MIME, Data: body}, nil
	case KindFiles, KindText:
		return Item{Kind: h.Kind, MIME: h.MIME, Text: string(body)}, nil
	}
	return Item{}, errors.New("unknown item kind")
}
