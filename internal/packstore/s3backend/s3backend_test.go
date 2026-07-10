package s3backend_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"

	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/localdir"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/s3backend"
	"github.com/CarriedWorldUniverse/porter/internal/s3"
	"github.com/CarriedWorldUniverse/porter/internal/s3/s3test"
)

func testCreds(bucket string) s3.Credentials {
	return s3.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "test-secret-access-key",
		Bucket:          bucket,
		Region:          "auto",
	}
}

// newTestClient wires an *s3.Client to a fake s3test server.
func newTestClient(t *testing.T, bucket string) (*s3.Client, *s3test.Server) {
	t.Helper()
	creds := testCreds(bucket)
	fake := s3test.New(bucket, creds)
	creds.Endpoint = fake.URL()
	return s3.New(creds, nil), fake
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

func TestBackendContract(t *testing.T) {
	c, fake := newTestClient(t, "packstore-bucket")
	defer fake.Close()
	ctx := context.Background()

	b := s3backend.New(ctx, c, "store-a")

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

	// Put on an existing name refuses, wrapping packstore.ErrExists.
	err = b.Put("pack-b", []byte("overwrite"))
	if !errors.Is(err, packstore.ErrExists) {
		t.Fatalf("Put(pack-b) again: got %v, want packstore.ErrExists", err)
	}

	// Get / Delete of a missing name error.
	if _, err := b.Get("nope"); err == nil {
		t.Fatal("Get(nope): want error, got nil")
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

	// A second store under a different prefix on the same bucket is
	// isolated: its List never sees store-a's objects.
	b2 := s3backend.New(ctx, c, "store-b")
	if err := b2.Put("pack-only-in-b", []byte("z")); err != nil {
		t.Fatalf("b2.Put: %v", err)
	}
	names, err = b.List("")
	if err != nil {
		t.Fatalf("b.List(\"\"): %v", err)
	}
	for _, n := range names {
		if n == "pack-only-in-b" {
			t.Fatalf("store-a List leaked store-b's object: %v", names)
		}
	}
}

// TestFullStoreRoundTripOverS3 drives packstore.Init/Put/Commit against an
// s3backend, then opens a fresh s3backend with only the second recipient's
// key and checks every artifact comes back byte-identical.
func TestFullStoreRoundTripOverS3(t *testing.T) {
	c, fake := newTestClient(t, "packstore-bucket")
	defer fake.Close()
	ctx := context.Background()

	privA, pubA := keypair(t)
	privB, pubB := keypair(t)
	recipients := [][]byte{pubA, pubB}

	const chunkSize = 16 * 1024
	const packSize = 64 * 1024

	b1 := s3backend.New(ctx, c, "store")
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

	b2 := s3backend.New(ctx, c, "store")
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
	_ = privA
}

// TestMirrorLocaldirToS3Backend mirrors a localdir store onto an
// s3backend, restores from the mirror, and checks a re-mirror is a no-op.
func TestMirrorLocaldirToS3Backend(t *testing.T) {
	c, fake := newTestClient(t, "packstore-bucket")
	defer fake.Close()
	ctx := context.Background()

	dir := t.TempDir()
	src, err := localdir.New(dir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}

	priv, pub := keypair(t)
	recipients := [][]byte{pub}
	const chunkSize = 16 * 1024
	const packSize = 64 * 1024

	w, err := packstore.Init(src, recipients, packSize, chunkSize)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	artifact := randomBytes(t, 100*1024+3)
	w.Put("artifact", artifact)
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dst := s3backend.New(ctx, c, "mirror-target")
	copied, skipped, err := packstore.Mirror(src, dst)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if copied == 0 {
		t.Fatal("Mirror: copied 0 objects")
	}
	if skipped != 0 {
		t.Fatalf("Mirror (first pass): skipped %d, want 0", skipped)
	}

	r, err := packstore.OpenReader(dst, priv)
	if err != nil {
		t.Fatalf("OpenReader on mirrored store: %v", err)
	}
	got, err := r.Get("artifact")
	if err != nil {
		t.Fatalf("Get(artifact) from mirror: %v", err)
	}
	if sha256Hex(got) != sha256Hex(artifact) {
		t.Fatal("Get(artifact) from mirror: content mismatch")
	}

	// Re-mirroring is a no-op: everything is already present.
	copied2, skipped2, err := packstore.Mirror(src, dst)
	if err != nil {
		t.Fatalf("Mirror (second pass): %v", err)
	}
	if copied2 != 0 {
		t.Fatalf("Mirror (second pass): copied %d, want 0", copied2)
	}
	if skipped2 == 0 {
		t.Fatalf("Mirror (second pass): skipped %d, want >0", skipped2)
	}
}
