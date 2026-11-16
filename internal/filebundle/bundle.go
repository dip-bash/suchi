// Package filebundle creates and safely extracts gzip-compressed tar bundles.
// The wire format is compatible with xclip-copyfile / xclip-pastefile.
package filebundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Create builds a gzip-compressed tar archive containing the named files and
// directories. Each input is stored under its basename, matching the default
// xclip-copyfile layout. Directory contents are included recursively.
func Create(inputs []string) ([]byte, error) {
	roots, err := normaliseInputs(inputs)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := createTo(&buf, roots); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CreateTo streams a gzip-compressed tar archive to w. It avoids buffering
// large media files in memory when the destination clipboard can stream data.
func CreateTo(w io.Writer, inputs []string) error {
	roots, err := normaliseInputs(inputs)
	if err != nil {
		return err
	}
	return createTo(w, roots)
}

func createTo(out io.Writer, roots []inputRoot) error {
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	for _, root := range roots {
		if err := addPath(tw, root.path, root.name); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return fmt.Errorf("finish tar archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finish gzip archive: %w", err)
	}
	return nil
}

type inputRoot struct {
	path string
	name string
}

func normaliseInputs(inputs []string) ([]inputRoot, error) {
	if len(inputs) == 0 {
		return nil, errors.New("provide at least one file or directory")
	}
	roots := make([]inputRoot, 0, len(inputs))
	seenNames := make(map[string]bool)
	for _, input := range inputs {
		if strings.TrimSpace(input) == "" {
			return nil, errors.New("empty input path")
		}
		abs, err := filepath.Abs(input)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", input, err)
		}
		if _, err := os.Lstat(abs); err != nil {
			return nil, fmt.Errorf("stat %q: %w", input, err)
		}
		name := filepath.Base(abs)
		if name == "." || name == string(filepath.Separator) || name == ".." || name == "" {
			return nil, fmt.Errorf("cannot bundle unsafe path %q", input)
		}
		if seenNames[name] {
			return nil, fmt.Errorf("multiple inputs would create the same destination %q", name)
		}
		seenNames[name] = true
		for _, previous := range roots {
			if pathContains(previous.path, abs) || pathContains(abs, previous.path) {
				return nil, fmt.Errorf("input paths overlap: %q and %q", previous.path, abs)
			}
		}
		roots = append(roots, inputRoot{path: abs, name: filepath.ToSlash(name)})
	}
	return roots, nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func addPath(tw *tar.Writer, diskPath, archiveName string) error {
	return filepath.WalkDir(diskPath, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(diskPath, current)
		if err != nil {
			return err
		}
		name := archiveName
		if rel != "." {
			name = path.Join(archiveName, filepath.ToSlash(rel))
		}

		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(current)
			if err != nil {
				return err
			}
			if err := safeSymlinkTarget(name, link); err != nil {
				return fmt.Errorf("symbolic link %s cannot be transferred safely: %w", current, err)
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type: %s", current)
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("create tar header for %s: %w", current, err)
		}
		hdr.Name = name
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write tar header for %s: %w", current, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(current)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("archive %s: %w", current, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	})
}

// Extract unpacks a gzip-compressed tar archive into an existing directory.
// It rejects traversal paths, special files and archive links that escape the
// destination. Existing top-level names are never overwritten. Extraction is
// staged and committed by rename so a malformed archive does not leave a
// partially extracted tree in the destination.
func Extract(data []byte, destination string) ([]string, error) {
	if len(data) == 0 {
		return nil, errors.New("the file-transfer clipboard is empty")
	}
	return ExtractReader(bytes.NewReader(data), destination)
}

// ExtractReader unpacks a gzip-compressed tar archive from src. See Extract
// for the safety and overwrite guarantees.
func ExtractReader(src io.Reader, destination string) ([]string, error) {
	if src == nil {
		return nil, errors.New("the file-transfer clipboard is empty")
	}
	dest, err := filepath.Abs(destination)
	if err != nil {
		return nil, fmt.Errorf("resolve destination: %w", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", destination, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("destination %q is not a directory", destination)
	}
	if real, err := filepath.EvalSymlinks(dest); err == nil {
		dest = real
	}

	gz, err := gzip.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("clipboard data is not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	stage, err := os.MkdirTemp(dest, ".suchi-paste-")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	roots := make(map[string]struct{})
	var symlinks []tar.Header
	var hardlinks []tar.Header
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar archive: %w", err)
		}
		rel, err := safeArchivePath(hdr.Name)
		if err != nil {
			return nil, err
		}
		first := strings.Split(filepath.ToSlash(rel), "/")[0]
		roots[first] = struct{}{}
		target := filepath.Join(stage, rel)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, safeDirMode(hdr.Mode)); err != nil {
				return nil, fmt.Errorf("create directory %q: %w", hdr.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > (8<<30)-total {
				return nil, errors.New("archive expands beyond the 8 GiB safety limit")
			}
			total += hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, safeFileMode(hdr.Mode))
			if err != nil {
				return nil, fmt.Errorf("create file %q: %w", hdr.Name, err)
			}
			_, copyErr := io.CopyN(f, tr, hdr.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return nil, fmt.Errorf("extract %q: %w", hdr.Name, copyErr)
			}
			if closeErr != nil {
				return nil, closeErr
			}
		case tar.TypeSymlink:
			if err := safeSymlinkTarget(rel, hdr.Linkname); err != nil {
				return nil, fmt.Errorf("unsafe symbolic link %q: %w", hdr.Name, err)
			}
			symlinks = append(symlinks, *hdr)
		case tar.TypeLink:
			if _, err := safeArchivePath(hdr.Linkname); err != nil {
				return nil, fmt.Errorf("unsafe hard link %q: %w", hdr.Name, err)
			}
			hardlinks = append(hardlinks, *hdr)
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			// archive/tar normally consumes these internally; tolerate a
			// metadata-only header if one is returned by a nonstandard writer.
		default:
			return nil, fmt.Errorf("archive entry %q has unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("archive contains no files")
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, fmt.Errorf("verify gzip archive: %w", err)
	}

	// Resolve hard links after regular files have been written. GNU tar
	// normally points links at an earlier archive member, but a few producers
	// emit forward references, so retry while progress is being made.
	for len(hardlinks) > 0 {
		remaining := hardlinks[:0]
		progress := false
		for _, hdr := range hardlinks {
			rel, _ := safeArchivePath(hdr.Name)
			linkRel, _ := safeArchivePath(hdr.Linkname)
			target := filepath.Join(stage, linkRel)
			info, err := os.Lstat(target)
			if err != nil || !info.Mode().IsRegular() {
				remaining = append(remaining, hdr)
				continue
			}
			linkPath := filepath.Join(stage, rel)
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				return nil, err
			}
			if err := os.Link(target, linkPath); err != nil {
				return nil, fmt.Errorf("create hard link %q: %w", hdr.Name, err)
			}
			progress = true
		}
		if !progress {
			return nil, errors.New("archive contains a hard link with a missing or invalid target")
		}
		hardlinks = remaining
	}

	// Create symlinks only after regular entries so archive paths cannot use
	// an earlier symlink as a directory and escape the staging tree.
	for _, hdr := range symlinks {
		rel, _ := safeArchivePath(hdr.Name)
		linkPath := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(linkPath); err == nil {
			return nil, fmt.Errorf("archive link %q conflicts with another entry", hdr.Name)
		}
		if err := os.Symlink(hdr.Linkname, linkPath); err != nil {
			return nil, fmt.Errorf("create symbolic link %q: %w", hdr.Name, err)
		}
	}

	names := make([]string, 0, len(roots))
	for name := range roots {
		if _, err := os.Lstat(filepath.Join(dest, name)); err == nil {
			return nil, fmt.Errorf("destination already contains %q; nothing was pasted", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("check destination %q: %w", name, err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	moved := make([]string, 0, len(names))
	for _, name := range names {
		from, to := filepath.Join(stage, name), filepath.Join(dest, name)
		if err := os.Rename(from, to); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = os.Rename(moved[i], filepath.Join(stage, filepath.Base(moved[i])))
			}
			return nil, fmt.Errorf("place %q in destination: %w", name, err)
		}
		moved = append(moved, to)
	}
	return names, nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive contains an unsafe path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive contains a path outside the destination: %q", name)
	}
	rel := filepath.FromSlash(clean)
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("archive contains a path outside the destination: %q", name)
	}
	return rel, nil
}

func safeSymlinkTarget(linkPath, target string) error {
	if target == "" || path.IsAbs(target) || strings.ContainsRune(target, '\x00') {
		return errors.New("link target must be a relative path")
	}
	resolved := path.Clean(path.Join(path.Dir(filepath.ToSlash(linkPath)), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
		return errors.New("link target escapes the destination")
	}
	return nil
}

func safeFileMode(mode int64) os.FileMode {
	return os.FileMode(mode&0o777) | 0o600
}

func safeDirMode(mode int64) os.FileMode {
	return os.FileMode(mode&0o777) | 0o700
}

// RemoveSources deletes cut inputs recursively. It refuses to remove the
// filesystem root, the user's home directory, or the current working
// directory (or any ancestor of either).
func RemoveSources(inputs []string) error {
	roots, err := normaliseInputs(inputs)
	if err != nil {
		return err
	}
	if err := validateRemoveRoots(roots); err != nil {
		return err
	}
	for _, root := range roots {
		if err := os.RemoveAll(root.path); err != nil {
			return fmt.Errorf("remove cut source %q: %w", root.path, err)
		}
	}
	return nil
}

// ValidateRemoveSources checks whether inputs are safe for a cut operation
// without deleting anything. Call it before publishing the clipboard data.
func ValidateRemoveSources(inputs []string) error {
	roots, err := normaliseInputs(inputs)
	if err != nil {
		return err
	}
	return validateRemoveRoots(roots)
}

func validateRemoveRoots(roots []inputRoot) error {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	protected := []string{string(filepath.Separator)}
	if cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil {
			protected = append(protected, abs)
		}
	}
	if home != "" {
		if abs, err := filepath.Abs(home); err == nil {
			protected = append(protected, abs)
		}
	}
	for _, root := range roots {
		for _, p := range protected {
			if pathContains(root.path, p) {
				return fmt.Errorf("refusing to cut %q because it contains a protected directory", root.path)
			}
		}
	}
	return nil
}
