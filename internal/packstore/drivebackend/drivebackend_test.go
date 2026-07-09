package drivebackend_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"google.golang.org/api/option"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/drive/drivetest"
	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/drivebackend"
)

// newTestClient wires a *drive.Client to a fake Drive server, matching
// internal/drive/drive_test.go's pattern.
func newTestClient(t *testing.T, fake *drivetest.Server) *drive.Client {
	t.Helper()
	c, err := drive.New(context.Background(), nil,
		option.WithEndpoint(fake.URL()), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("drive.New: %v", err)
	}
	return c
}

func keypair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatalf("GenerateRecipientKey: %v", err)
	}
	return priv, pub
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

func newFolder(t *testing.T, fake *drivetest.Server, c *drive.Client) string {
	t.Helper()
	ctx := context.Background()
	id, err := c.EnsureFolder(ctx, "packstore-test")
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}
	return id
}

func TestBackendContract(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder := newFolder(t, fake, c)

	b := drivebackend.New(ctx, c, folder)

	if err := b.Put("pack-b", []byte("BBBB")); err != nil {
		t.Fatalf("Put(pack-b): %v", err)
	}
	if err := b.Put("sb-2", []byte("sb2")); err != nil {
		t.Fatalf("Put(sb-2): %v", err)
	}
	if err := b.Put("pack-a", []byte("AAAA")); err != nil {
		t.Fatalf("Put(pack-a): %v", err)
	}
	if err := b.Put("idx-1", []byte("idx1")); err != nil {
		t.Fatalf("Put(idx-1): %v", err)
	}
	if err := b.Put("pack-c", []byte("CCCC")); err != nil {
		t.Fatalf("Put(pack-c): %v", err)
	}

	// Round trip.
	got, err := b.Get("pack-b")
	if err != nil {
		t.Fatalf("Get(pack-b): %v", err)
	}
	if string(got) != "BBBB" {
		t.Fatalf("Get(pack-b): got %q", got)
	}

	// Put on an existing name errors.
	if err := b.Put("pack-b", []byte("overwrite")); err == nil {
		t.Fatal("Put(pack-b) again: want error, got nil")
	}

	// Get / Delete of a missing name error, and mention the name.
	if _, err := b.Get("nope"); err == nil {
		t.Fatal("Get(nope): want error, got nil")
	}
	if err := b.Delete("nope"); err == nil {
		t.Fatal("Delete(nope): want error, got nil")
	}

	// List filters by prefix and sorts, regardless of insertion order.
	names, err := b.List("pack-")
	if err != nil {
		t.Fatalf("List(pack-): %v", err)
	}
	want := []string{"pack-a", "pack-b", "pack-c"}
	if !sort.StringsAreSorted(names) || len(names) != len(want) {
		t.Fatalf("List(pack-): got %v, want sorted %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("List(pack-): got %v, want %v", names, want)
		}
	}

	// Delete round trip.
	if err := b.Delete("pack-a"); err != nil {
		t.Fatalf("Delete(pack-a): %v", err)
	}
	if _, err := b.Get("pack-a"); err == nil {
		t.Fatal("Get(pack-a) after delete: want error, got nil")
	}
}

// TestFullStoreRoundTripOverDrive drives packstore.Init/Put/Commit against a
// drivebackend, then opens a FRESH drivebackend (empty cache, forcing name
// resolution through the fake) with only the second recipient's key and
// checks every artifact comes back byte-identical.
func TestFullStoreRoundTripOverDrive(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder := newFolder(t, fake, c)

	privA, pubA := keypair(t)
	privB, pubB := keypair(t)
	recipients := [][]byte{pubA, pubB}

	const chunkSize = 16 * 1024
	const packSize = 64 * 1024

	b1 := drivebackend.New(ctx, c, folder)
	w, err := packstore.Init(b1, recipients, packSize, chunkSize)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	artifacts := map[string][]byte{
		"empty":       {},
		"one-byte":    randomBytes(t, 1),
		"exact-chunk": randomBytes(t, chunkSize+1),
		"multi-pack":  randomBytes(t, 200*1024+7),
	}
	first := []string{"empty", "one-byte"}
	second := []string{"exact-chunk", "multi-pack"}

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

	// Fresh backend instance -> empty cache -> Get must resolve names
	// through the fake server from scratch.
	b2 := drivebackend.New(ctx, c, folder)
	r, err := packstore.OpenReader(b2, privB)
	if err != nil {
		t.Fatalf("OpenReader(privB): %v", err)
	}
	for name, want := range artifacts {
		got, err := r.Get(name)
		if err != nil {
			t.Fatalf("Get(%s) via privB: %v", name, err)
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("Get(%s) via privB: mismatch (len got=%d want=%d)", name, len(got), len(want))
		}
	}

	// TestPackUniformityOnDrive continuation: every pack-* object on Drive
	// is the same byte size.
	var size int64 = -1
	for _, f := range fake.Files() {
		if len(f.Name) < 5 || f.Name[:5] != "pack-" {
			continue
		}
		if size == -1 {
			size = int64(len(f.Content))
			continue
		}
		if int64(len(f.Content)) != size {
			t.Fatalf("pack %s size %d != first pack size %d", f.Name, len(f.Content), size)
		}
	}
	if size == -1 {
		t.Fatal("no pack-* objects found on fake Drive")
	}
	_ = privA
}

// TestCacheMissRefresh checks that a file created via a second, separate
// drivebackend instance is still visible to the first instance's Get (cache
// refresh on miss).
func TestCacheMissRefresh(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder := newFolder(t, fake, c)

	b1 := drivebackend.New(ctx, c, folder)
	b2 := drivebackend.New(ctx, c, folder)

	if err := b2.Put("late-arrival", []byte("hello")); err != nil {
		t.Fatalf("b2.Put: %v", err)
	}

	// b1 has never seen "late-arrival" — its cache is empty — but Get must
	// refresh once on the miss and find it.
	got, err := b1.Get("late-arrival")
	if err != nil {
		t.Fatalf("b1.Get(late-arrival): %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("b1.Get(late-arrival): got %q", got)
	}
}
