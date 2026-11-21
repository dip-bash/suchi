// Package fileop copies and moves files and folders for `suchi -p`.
package fileop

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Paste copies (or moves, when cut) paths into destDir. It refuses to
// overwrite an existing destination and returns the new paths.
func Paste(paths []string, destDir string, cut bool) ([]string, error) {
	if destDir == "" {
		destDir = "."
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absDest)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", absDest)
	}
	// Resolve symlinks so pasting into a link to a folder inside the source
	// is caught by the recursion check below.
	if resolved, err := filepath.EvalSymlinks(absDest); err == nil {
		absDest = resolved
	}

	type job struct{ src, dst string }
	jobs := make([]job, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	destSeen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		src, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if seen[src] {
			continue
		}
		seen[src] = true
		if _, err := os.Lstat(src); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		dst := filepath.Join(absDest, filepath.Base(src))
		if destSeen[dst] {
			return nil, fmt.Errorf("two items would both paste as %s", dst)
		}
		destSeen[dst] = true
		if filepath.Clean(src) == filepath.Clean(dst) {
			return nil, fmt.Errorf("%s is already in %s", src, absDest)
		}
		if inside(src, dst) {
			return nil, fmt.Errorf("cannot paste %s into itself", src)
		}
		if _, err := os.Lstat(dst); err == nil {
			return nil, fmt.Errorf("%s already exists", dst)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		jobs = append(jobs, job{src, dst})
	}
	if len(jobs) == 0 {
		return nil, errors.New("no files to paste")
	}

	done := make([]string, 0, len(jobs))
	for _, j := range jobs {
		var err error
		if cut {
			err = Move(j.src, j.dst)
		} else {
			err = Copy(j.src, j.dst)
		}
		if err != nil {
			return done, fmt.Errorf("%s: %w", j.src, err)
		}
		done = append(done, j.dst)
	}
	return done, nil
}

// inside reports whether child is strictly inside parent.
func inside(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Copy copies a file, folder tree or symlink. Symlinks are recreated as
// symlinks instead of being followed.
func Copy(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := Copy(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(dst, info.Mode().Perm())
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return os.Chmod(dst, info.Mode().Perm())
	default:
		return fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
}

// Move renames src to dst, falling back to copy-and-delete across filesystems.
func Move(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := Copy(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}
