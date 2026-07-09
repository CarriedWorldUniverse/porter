package packstore_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"

	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/localdir"
)

// keypair mints a fresh X25519 recipient keypair for a test.
func keypair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatalf("GenerateRecipientKey: %v", err)
	}
	return priv, pub
}

// newBackend opens a fresh localdir backend rooted at a fresh temp dir.
func newBackend(t *testing.T) *localdir.Dir {
	t.Helper()
	b, err := localdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	return b
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}

// packFiles lists the "pack-*" object file names under a localdir backend's
// root, for tests that need to inspect the backend directly.
func packFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "pack-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestRoundTripAcrossMultipleCommits(t *testing.T) {
	dir := t.TempDir()
	b, err := localdir.New(dir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	priv, pub := keypair(t)
	recipients := [][]byte{pub}

	const chunkSize = 64 * 1024
	const packSize = 256 * 1024

	w, err := packstore.Init(b, recipients, packSize, chunkSize)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	artifacts := map[string][]byte{
		"empty":        {},
		"one-byte":     randomBytes(t, 1),
		"exact-chunk":  randomBytes(t, chunkSize),
		"chunk-plus-1": randomBytes(t, chunkSize+1),
		"multi-pack":   randomBytes(t, 3*1024*1024+512*1024+7), // ~3.5 MiB, spans multiple packs
	}

	// Split across two commits so the round trip exercises multiple
	// generations.
	first := []string{"empty", "one-byte", "exact-chunk"}
	second := []string{"chunk-plus-1", "multi-pack"}

	for _, name := range first {
		w.Put(name, artifacts[name])
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit (first): %v", err)
	}
	for _, name := range second {
		w.Put(name, artifacts[name])
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit (second): %v", err)
	}

	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	for name, want := range artifacts {
		got, err := r.Get(name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("Get(%s): sha256 mismatch (len got=%d want=%d)", name, len(got), len(want))
		}
	}
}

func TestRecoveryWithSingleRecipientKey(t *testing.T) {
	dir := t.TempDir()
	b, err := localdir.New(dir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	privA, pubA := keypair(t)
	privB, pubB := keypair(t)
	recipients := [][]byte{pubA, pubB}

	w, err := packstore.Init(b, recipients, 256*1024, 64*1024)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	artifacts := map[string][]byte{
		"a": randomBytes(t, 100*1024),
		"b": randomBytes(t, 10),
	}
	for name, data := range artifacts {
		w.Put(name, data)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Open a FRESH reader with only B's key.
	r, err := packstore.OpenReader(b, privB)
	if err != nil {
		t.Fatalf("OpenReader(privB): %v", err)
	}
	for name, want := range artifacts {
		got, err := r.Get(name)
		if err != nil {
			t.Fatalf("Get(%s) via privB: %v", name, err)
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("Get(%s) via privB: mismatch", name)
		}
	}

	// privA also still works (sanity: it's genuinely multi-recipient).
	rA, err := packstore.OpenReader(b, privA)
	if err != nil {
		t.Fatalf("OpenReader(privA): %v", err)
	}
	if got, err := rA.Get("a"); err != nil || sha256Hex(got) != sha256Hex(artifacts["a"]) {
		t.Fatalf("Get(a) via privA: got=%v err=%v", got, err)
	}
}

func TestPackUniformity(t *testing.T) {
	dir := t.TempDir()
	b, err := localdir.New(dir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	_, pub := keypair(t)
	recipients := [][]byte{pub}

	const packSize = 128 * 1024
	w, err := packstore.Init(b, recipients, packSize, 32*1024)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	w.Put("a", randomBytes(t, 40*1024))
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	w.Put("b", randomBytes(t, 300*1024))
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit (2): %v", err)
	}

	names := packFiles(t, dir)
	if len(names) == 0 {
		t.Fatal("no pack-* objects found")
	}
	var size int64 = -1
	for _, name := range names {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("Stat(%s): %v", name, err)
		}
		if size == -1 {
			size = info.Size()
			continue
		}
		if info.Size() != size {
			t.Fatalf("pack %s size %d != first pack size %d", name, info.Size(), size)
		}
	}
}

func TestDedupAcrossNamesAndCommits(t *testing.T) {
	dir := t.TempDir()
	b, err := localdir.New(dir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	_, pub := keypair(t)
	recipients := [][]byte{pub}

	w, err := packstore.Init(b, recipients, 128*1024, 32*1024)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	content := randomBytes(t, 100*1024)

	w.Put("name-one", content)
	w.Put("name-two", content) // identical content, different artifact name
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit (1): %v", err)
	}
	before := len(packFiles(t, dir))
	if before == 0 {
		t.Fatal("expected at least one pack after first commit")
	}

	// Second commit: same content again, under a third name. Should dedup
	// entirely against the existing index -> zero new packs.
	w.Put("name-three", content)
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit (2): %v", err)
	}
	after := len(packFiles(t, dir))
	if after != before {
		t.Fatalf("second commit created %d new pack objects, want 0 (before=%d after=%d)", after-before, before, after)
	}
}

func TestTamperedPackFailsGet(t *testing.T) {
	dir := t.TempDir()
	b, err := localdir.New(dir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	priv, pub := keypair(t)
	recipients := [][]byte{pub}

	w, err := packstore.Init(b, recipients, 128*1024, 32*1024)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	w.Put("victim", randomBytes(t, 100*1024))
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	names := packFiles(t, dir)
	if len(names) == 0 {
		t.Fatal("no pack objects to tamper with")
	}
	path := filepath.Join(dir, names[0])
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	mid := len(data) / 2
	data[mid] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	if _, err := r.Get("victim"); err == nil {
		t.Fatal("Get on artifact backed by a tampered pack: want error, got nil")
	}
}

func TestOrphanPackHarmless(t *testing.T) {
	dir := t.TempDir()
	b, err := localdir.New(dir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	priv, pub := keypair(t)
	recipients := [][]byte{pub}

	w, err := packstore.Init(b, recipients, 128*1024, 32*1024)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	artifacts := map[string][]byte{
		"a": randomBytes(t, 50*1024),
		"b": randomBytes(t, 200*1024),
	}
	for name, data := range artifacts {
		w.Put(name, data)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Simulate a crash that left an unreferenced pack behind: a
	// well-formed sealed object under the pack-* naming convention that no
	// index entry points to.
	garbage := randomBytes(t, 128*1024)
	if err := b.Put("pack-orphandeadbeefdeadbeefdeadbeef00000001", garbage); err != nil {
		t.Fatalf("Put(orphan pack): %v", err)
	}

	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	for name, want := range artifacts {
		got, err := r.Get(name)
		if err != nil {
			t.Fatalf("Get(%s) with orphan pack present: %v", name, err)
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("Get(%s) with orphan pack present: mismatch", name)
		}
	}
}

func TestLocaldirPutRefusesOverwrite(t *testing.T) {
	b := newBackend(t)
	if err := b.Put("obj-1", []byte("first")); err != nil {
		t.Fatalf("Put(first): %v", err)
	}
	if err := b.Put("obj-1", []byte("second")); err == nil {
		t.Fatal("Put(overwrite): want error, got nil")
	}
	got, err := b.Get("obj-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("Get: got %q, want original content preserved (%q)", got, "first")
	}
}
