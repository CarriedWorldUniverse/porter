package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// snapshotTar archives the directory at root into a DETERMINISTIC tar.gz at
// dst: identical tree state in → identical bytes out. Determinism comes from
// (a) filepath.WalkDir's lexical ordering, (b) a zeroed gzip header (no
// embedded mtime/name), and (c) tar headers stripped of atime/ctime and
// user/group names (mode + mtime are kept — they are tree state a restore
// wants back).
//
// excludes are glob patterns (path.Match syntax, per segment) tested against
// each entry's slash-relative path AND its base name; matching a directory
// prunes its whole subtree. Examples: "src" (subtree), "*.tmp" (any depth),
// "work/cache" (specific subtree).
//
// Regular files, directories, and symlinks are archived; sockets/devices/
// fifos are skipped (croft home has live sockets — they are not backup
// state). Symlink targets are recorded, never followed.
func snapshotTar(root, dst string, excludes []string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("tar source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("tar source %s: not a directory", root)
	}

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating tar artifact: %w", err)
	}
	defer f.Close()

	// gzip.Writer with an untouched zero header (no ModTime, no Name) is
	// byte-deterministic for identical input.
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if excluded(rel, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}
		var linkTarget string
		switch {
		case fi.Mode().IsRegular(), fi.IsDir():
		case fi.Mode()&fs.ModeSymlink != 0:
			if linkTarget, err = os.Readlink(p); err != nil {
				return err
			}
		default:
			return nil // socket/device/fifo: not backup state
		}

		hdr, err := tar.FileInfoHeader(fi, linkTarget)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if fi.IsDir() {
			hdr.Name += "/"
		}
		// Determinism: strip fields that change without tree content
		// changing (or that PAX would otherwise serialize).
		hdr.AccessTime, hdr.ChangeTime = hdr.ModTime, hdr.ModTime
		hdr.Uname, hdr.Gname = "", ""
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			// Write EXACTLY hdr.Size bytes — a point-in-time snapshot of the file
			// as of the stat above. A live file (e.g. a session .jsonl an active
			// agent is appending to) can grow or shrink between stat and read, but
			// archive/tar demands precisely hdr.Size bytes: a plain io.Copy of the
			// whole file would overrun on growth ("write too long") and fail the
			// ENTIRE backup. io.CopyN caps the read at hdr.Size (dropping any
			// concurrent growth); a concurrent shrink is zero-padded so the entry
			// still matches its header.
			n, cerr := io.CopyN(tw, src, hdr.Size)
			src.Close()
			if cerr == io.EOF {
				if _, perr := tw.Write(make([]byte, hdr.Size-n)); perr != nil {
					return fmt.Errorf("padding %s: %w", rel, perr)
				}
			} else if cerr != nil {
				return fmt.Errorf("archiving %s: %w", rel, cerr)
			}
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walking %s: %w", root, walkErr)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finalizing tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finalizing gzip: %w", err)
	}
	return f.Close()
}

// excluded reports whether a slash-relative path is pruned by the exclude
// globs: a pattern matching the path itself, any ancestor (subtree prune), or
// the entry's base name excludes it.
func excluded(rel string, excludes []string) bool {
	for _, pat := range excludes {
		pat = strings.TrimSuffix(pat, "/")
		if ok, _ := path.Match(pat, path.Base(rel)); ok {
			return true
		}
		for p := rel; p != "."; p = path.Dir(p) {
			if ok, _ := path.Match(pat, p); ok {
				return true
			}
		}
	}
	return false
}
