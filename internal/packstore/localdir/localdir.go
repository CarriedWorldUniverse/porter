// Package localdir is a packstore.Backend backed by flat files in a local
// directory. It is the "experimental scale" backend: no cloud calls, no
// concurrency control beyond what a single writer needs. Put refuses to
// overwrite an existing object and writes atomically (temp file + rename)
// so a crash mid-write never leaves a partial object visible under its
// final name.
package localdir

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Dir is a packstore.Backend rooted at a local directory.
type Dir struct {
	root string
}

// New opens (creating if necessary) a local-directory backend rooted at
// path.
func New(path string) (*Dir, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("packstore/localdir: mkdir %s: %w", path, err)
	}
	return &Dir{root: path}, nil
}

// Put writes name atomically (temp file + rename) and refuses to overwrite
// an existing object.
func (d *Dir) Put(name string, data []byte) error {
	final := filepath.Join(d.root, name)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("packstore/localdir: put %s: already exists", name)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("packstore/localdir: put %s: stat: %w", name, err)
	}

	tmp, err := os.CreateTemp(d.root, ".tmp-"+name+"-*")
	if err != nil {
		return fmt.Errorf("packstore/localdir: put %s: create temp: %w", name, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("packstore/localdir: put %s: write: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("packstore/localdir: put %s: close: %w", name, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("packstore/localdir: put %s: rename: %w", name, err)
	}
	return nil
}

// Get reads name's full contents.
func (d *Dir) Get(name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(d.root, name))
	if err != nil {
		return nil, fmt.Errorf("packstore/localdir: get %s: %w", name, err)
	}
	return data, nil
}

// List returns the lexicographically sorted names of objects whose name
// starts with prefix. Temp files from an in-progress Put are never listed.
func (d *Dir) List(prefix string) ([]string, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, fmt.Errorf("packstore/localdir: list %q: %w", prefix, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") {
			continue
		}
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes name.
func (d *Dir) Delete(name string) error {
	if err := os.Remove(filepath.Join(d.root, name)); err != nil {
		return fmt.Errorf("packstore/localdir: delete %s: %w", name, err)
	}
	return nil
}
