package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if err := snapshotTar(root, out, nil, nil, 0); err != nil {
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
	if err := snapshotTar(root, a, nil, nil, 0); err != nil {
		t.Fatalf("snapshotTar a: %v", err)
	}
	if err := snapshotTar(root, b, nil, nil, 0); err != nil {
		t.Fatalf("snapshotTar b: %v", err)
	}
	if sha256File(t, a) != sha256File(t, b) {
		t.Fatal("two tars of the same unchanged tree differ — not deterministic")
	}
}

func TestSnapshotTarExcludes(t *testing.T) {
	root := fixtureTree(t)
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := snapshotTar(root, out, []string{"src", "work", "*.tmp"}, nil, 0); err != nil {
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

// TestSnapshotTarConcurrentGrowth is the live-session .jsonl case: a file an
// active agent keeps appending to while the snapshot runs. With a whole-file
// io.Copy this overran the tar header size ("write too long") and failed the
// entire croft-home backup — the reason transcripts were excluded. The
// point-in-time io.CopyN must instead archive the file's size-at-stat and let
// the backup complete.
func TestSnapshotTarConcurrentGrowth(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(big, make([]byte, 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.OpenFile(big, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		buf := make([]byte, 64<<10)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = f.Write(buf)
			}
		}
	}()
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	err := snapshotTar(root, out, nil, nil, 0)
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("snapshotTar must tolerate a file growing during archiving: %v", err)
	}
	// tar.Reader enforces each entry's byte-count == its header Size, so a clean
	// read proves the growing file was archived point-in-time, not overrun.
	if _, ok := readTarGz(t, out)["session.jsonl"]; !ok {
		t.Fatal("growing file missing from snapshot")
	}
}

func TestSnapshotTarMissingRoot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := snapshotTar(filepath.Join(t.TempDir(), "absent"), out, nil, nil, 0); err == nil {
		t.Fatal("want error for missing root")
	}
}

// allowlistTree mirrors the croft-home shape: a few precious paths amid bulk
// that should be ignored by DEFAULT under an allowlist.
func allowlistTree(t *testing.T) string {
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
	write("notes.md", "design doc")             // include via "*.md"
	write(".claude/skills/s.md", "a skill")     // include via ".claude"
	write(".claude/projects/x/sess.jsonl", "…") // under .claude but excluded
	write(".config/app.conf", "cfg")            // NOT included
	write("models/big.bin", "19G of weights")   // NOT included — the bulk
	write("deep/a/b/target.txt", "reached")     // include via slashed "deep/a/b"
	write("deep/a/other.txt", "sibling")        // sibling of b, NOT under include
	return root
}

func TestSnapshotTarIncludesAllowlist(t *testing.T) {
	root := allowlistTree(t)
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	includes := []string{".claude", "*.md", "deep/a/b"}
	excludes := []string{".claude/projects"}
	if err := snapshotTar(root, out, excludes, includes, 0); err != nil {
		t.Fatalf("snapshotTar: %v", err)
	}
	got := readTarGz(t, out)

	for _, want := range []string{"notes.md", ".claude/skills/s.md", "deep/a/b/target.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("allowlisted path missing from tar: %s", want)
		}
	}
	for _, unwanted := range []string{
		"models/big.bin",                // not in the allowlist → ignored by default
		".config/app.conf",              // not in the allowlist
		"deep/a/other.txt",              // sibling outside the included subtree
		".claude/projects/x/sess.jsonl", // in allowlist subtree but excluded subtracts
	} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("path outside the allowlist present in tar: %s", unwanted)
		}
	}
	// ancestor dirs on the way to a slashed include are archived for structure.
	if _, ok := got["deep/a/b/"]; !ok {
		t.Error("ancestor dir deep/a/b/ missing")
	}
}

func TestSnapshotTarEmptyIncludesArchivesAll(t *testing.T) {
	// Empty includes must be identical to block-list mode (backward compatible).
	root := allowlistTree(t)
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := snapshotTar(root, out, nil, nil, 0); err != nil {
		t.Fatalf("snapshotTar: %v", err)
	}
	got := readTarGz(t, out)
	if _, ok := got["models/big.bin"]; !ok {
		t.Fatal("empty includes should archive everything, incl models/big.bin")
	}
}

func TestSnapshotTarMaxBytesGuard(t *testing.T) {
	root := t.TempDir()
	// Incompressible payload so the gzip'd artifact can't shrink under the cap.
	blob := make([]byte, 1<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "capped.tar.gz")
	err := snapshotTar(root, out, nil, nil, 64<<10) // 64 KiB cap << 1 MiB payload
	if err == nil {
		t.Fatal("want error when the staged tar exceeds max_bytes")
	}
	if !strings.Contains(err.Error(), "exceeded max size") {
		t.Fatalf("error should name the size guard, got: %v", err)
	}

	// A generous cap must pass.
	out2 := filepath.Join(t.TempDir(), "ok.tar.gz")
	if err := snapshotTar(root, out2, nil, nil, 8<<20); err != nil {
		t.Fatalf("generous cap should pass: %v", err)
	}
}
