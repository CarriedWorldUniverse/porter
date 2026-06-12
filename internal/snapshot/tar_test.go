package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fixtureTree builds a directory with files, a subdir, a symlink, and
// content that the exclude tests target.
func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory/MEMORY.md", "memory contents")
	write("dotfiles/.bashrc", "alias ll='ls -l'")
	write("src/clone/big.go", "package clone // should be excludable")
	write("work/cache.bin", "cache")
	write("scratch.tmp", "tmp")
	write("deep/nested/keep.txt", "keep me")
	if err := os.Symlink("memory/MEMORY.md", filepath.Join(root, "memlink")); err != nil {
		t.Fatal(err)
	}
	return root
}

// readTarGz returns rel-path -> content for files, and rel-path -> target for
// symlinks (prefixed "symlink:"), plus dirs as "dir".
func readTarGz(t *testing.T, p string) map[string]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			out[hdr.Name] = "dir"
		case tar.TypeSymlink:
			out[hdr.Name] = "symlink:" + hdr.Linkname
		default:
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s: %v", hdr.Name, err)
			}
			out[hdr.Name] = string(b)
		}
	}
	return out
}

func sha256File(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestSnapshotTarContents(t *testing.T) {
	root := fixtureTree(t)
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := snapshotTar(root, out, nil); err != nil {
		t.Fatalf("snapshotTar: %v", err)
	}
	got := readTarGz(t, out)
	if got["memory/MEMORY.md"] != "memory contents" {
		t.Fatalf("memory/MEMORY.md: %q", got["memory/MEMORY.md"])
	}
	if got["dotfiles/.bashrc"] == "" {
		t.Fatal("dotfile missing")
	}
	if got["deep/nested/keep.txt"] != "keep me" {
		t.Fatal("nested file missing")
	}
	if got["memlink"] != "symlink:memory/MEMORY.md" {
		t.Fatalf("symlink: %q", got["memlink"])
	}
}

func TestSnapshotTarDeterministic(t *testing.T) {
	root := fixtureTree(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "b.tar.gz")
	if err := snapshotTar(root, a, nil); err != nil {
		t.Fatalf("snapshotTar a: %v", err)
	}
	if err := snapshotTar(root, b, nil); err != nil {
		t.Fatalf("snapshotTar b: %v", err)
	}
	if sha256File(t, a) != sha256File(t, b) {
		t.Fatal("two tars of the same unchanged tree differ — not deterministic")
	}
}

func TestSnapshotTarExcludes(t *testing.T) {
	root := fixtureTree(t)
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := snapshotTar(root, out, []string{"src", "work", "*.tmp"}); err != nil {
		t.Fatalf("snapshotTar: %v", err)
	}
	got := readTarGz(t, out)
	for path := range got {
		switch {
		case path == "src" || path == "work",
			filepath.Dir(path) == "src" || len(path) > 4 && path[:4] == "src/",
			len(path) > 5 && path[:5] == "work/",
			filepath.Ext(path) == ".tmp":
			t.Errorf("excluded path present in tar: %s", path)
		}
	}
	if got["memory/MEMORY.md"] == "" {
		t.Fatal("non-excluded file missing")
	}
}

func TestSnapshotTarMissingRoot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := snapshotTar(filepath.Join(t.TempDir(), "absent"), out, nil); err == nil {
		t.Fatal("want error for missing root")
	}
}
