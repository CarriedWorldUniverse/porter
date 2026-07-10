package packstore_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/api/option"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/drive/drivetest"
	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/drivebackend"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/localdir"
)

// buildStore writes a small multi-commit store into b and returns the
// recipient private key (single recipient) plus the artifacts written.
func buildStore(t *testing.T, b packstore.Backend) (priv []byte, artifacts map[string][]byte) {
	t.Helper()
	priv, pub := keypair(t)
	recipients := [][]byte{pub}

	const packSize = 64 * 1024
	const chunkSize = 16 * 1024

	w, err := packstore.Init(b, recipients, packSize, chunkSize)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	artifacts = map[string][]byte{
		"a": randomBytes(t, 1024),
		"b": randomBytes(t, 100*1024),
	}
	w.Put("a", artifacts["a"])
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit (1): %v", err)
	}
	w.Put("b", artifacts["b"])
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit (2): %v", err)
	}
	return priv, artifacts
}

// recordingBackend wraps a packstore.Backend and records the name of every
// object Put, in call order, for ordering assertions.
type recordingBackend struct {
	packstore.Backend
	puts []string
}

func (r *recordingBackend) Put(name string, data []byte) error {
	if err := r.Backend.Put(name, data); err != nil {
		return err
	}
	r.puts = append(r.puts, name)
	return nil
}

func TestMirrorLocaldirToLocaldir(t *testing.T) {
	src, err := localdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("localdir.New (src): %v", err)
	}
	priv, artifacts := buildStore(t, src)

	dst, err := localdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("localdir.New (dst): %v", err)
	}

	srcNames, err := src.List("")
	if err != nil {
		t.Fatalf("src.List: %v", err)
	}

	copied, skipped, err := packstore.Mirror(src, dst)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if copied != len(srcNames) || skipped != 0 {
		t.Fatalf("Mirror: got copied=%d skipped=%d, want copied=%d skipped=0", copied, skipped, len(srcNames))
	}

	dstNames, err := dst.List("")
	if err != nil {
		t.Fatalf("dst.List: %v", err)
	}
	if len(dstNames) != len(srcNames) {
		t.Fatalf("dst has %d objects, src has %d", len(dstNames), len(srcNames))
	}
	for _, name := range srcNames {
		want, err := src.Get(name)
		if err != nil {
			t.Fatalf("src.Get(%s): %v", name, err)
		}
		got, err := dst.Get(name)
		if err != nil {
			t.Fatalf("dst.Get(%s): %v", name, err)
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("object %s: byte mismatch after mirror", name)
		}
	}

	// Restore from the mirrored store proves it is a working store, not
	// just a bag of copied bytes.
	r, err := packstore.OpenReader(dst, priv)
	if err != nil {
		t.Fatalf("OpenReader(dst): %v", err)
	}
	for name, want := range artifacts {
		got, err := r.Get(name)
		if err != nil {
			t.Fatalf("dst reader Get(%s): %v", name, err)
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("dst reader Get(%s): mismatch", name)
		}
	}

	// Second Mirror: everything already present -> 0 copied.
	copied2, skipped2, err := packstore.Mirror(src, dst)
	if err != nil {
		t.Fatalf("Mirror (2): %v", err)
	}
	if copied2 != 0 || skipped2 != len(srcNames) {
		t.Fatalf("Mirror (2): got copied=%d skipped=%d, want copied=0 skipped=%d", copied2, skipped2, len(srcNames))
	}
}

func TestMirrorOrdering(t *testing.T) {
	src, err := localdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("localdir.New (src): %v", err)
	}
	buildStore(t, src)

	realDst, err := localdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("localdir.New (dst): %v", err)
	}
	dst := &recordingBackend{Backend: realDst}

	if _, _, err := packstore.Mirror(src, dst); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if len(dst.puts) == 0 {
		t.Fatal("Mirror recorded no Puts")
	}

	lastPackIdx, firstIdxIdx, lastIdxIdx, firstSBIdx := -1, -1, -1, -1
	for i, name := range dst.puts {
		switch {
		case len(name) >= 5 && name[:5] == "pack-":
			lastPackIdx = i
		case len(name) >= 4 && name[:4] == "idx-":
			if firstIdxIdx == -1 {
				firstIdxIdx = i
			}
			lastIdxIdx = i
		case len(name) >= 3 && name[:3] == "sb-":
			if firstSBIdx == -1 {
				firstSBIdx = i
			}
		}
	}
	if lastPackIdx == -1 || firstIdxIdx == -1 || firstSBIdx == -1 {
		t.Fatalf("expected pack-, idx- and sb- objects to all be mirrored: puts=%v", dst.puts)
	}
	if !(lastPackIdx < firstIdxIdx) {
		t.Fatalf("a pack- was Put at or after the first idx- Put: puts=%v", dst.puts)
	}
	if !(lastIdxIdx < firstSBIdx) {
		t.Fatalf("an idx- was Put at or after the first sb- Put: puts=%v", dst.puts)
	}
}

func TestPutErrExistsLocaldir(t *testing.T) {
	b := newBackend(t)
	if err := b.Put("obj-1", []byte("first")); err != nil {
		t.Fatalf("Put(first): %v", err)
	}
	err := b.Put("obj-1", []byte("second"))
	if err == nil {
		t.Fatal("Put(overwrite): want error, got nil")
	}
	if !errors.Is(err, packstore.ErrExists) {
		t.Fatalf("Put(overwrite): err = %v, want it to satisfy errors.Is(err, packstore.ErrExists)", err)
	}
}

func TestPutErrExistsDrivebackend(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	ctx := context.Background()
	c, err := drive.New(ctx, nil, option.WithEndpoint(fake.URL()), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("drive.New: %v", err)
	}
	folder, err := c.EnsureFolder(ctx, "mirror-errexists-test")
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}
	b := drivebackend.New(ctx, c, folder)

	if err := b.Put("obj-1", []byte("first")); err != nil {
		t.Fatalf("Put(first): %v", err)
	}
	err = b.Put("obj-1", []byte("second"))
	if err == nil {
		t.Fatal("Put(overwrite): want error, got nil")
	}
	if !errors.Is(err, packstore.ErrExists) {
		t.Fatalf("Put(overwrite): err = %v, want it to satisfy errors.Is(err, packstore.ErrExists)", err)
	}
}

func TestMirrorLocaldirToDriveThenRestore(t *testing.T) {
	src, err := localdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("localdir.New (src): %v", err)
	}
	priv, artifacts := buildStore(t, src)

	fake := drivetest.New()
	defer fake.Close()
	ctx := context.Background()
	c, err := drive.New(ctx, nil, option.WithEndpoint(fake.URL()), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("drive.New: %v", err)
	}
	folder, err := c.EnsureFolder(ctx, "mirror-dest-test")
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}
	dst := drivebackend.New(ctx, c, folder)

	srcNames, err := src.List("")
	if err != nil {
		t.Fatalf("src.List: %v", err)
	}
	copied, skipped, err := packstore.Mirror(src, dst)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if copied != len(srcNames) || skipped != 0 {
		t.Fatalf("Mirror: got copied=%d skipped=%d, want copied=%d skipped=0", copied, skipped, len(srcNames))
	}

	// Fresh backend instance over the same Drive folder -> empty cache ->
	// exercises real name resolution, matching drivebackend_test.go's own
	// pattern.
	dst2 := drivebackend.New(ctx, c, folder)
	r, err := packstore.OpenReader(dst2, priv)
	if err != nil {
		t.Fatalf("OpenReader(mirrored drive store): %v", err)
	}
	for name, want := range artifacts {
		got, err := r.Get(name)
		if err != nil {
			t.Fatalf("Get(%s) from mirrored drive store: %v", name, err)
		}
		if sha256Hex(got) != sha256Hex(want) {
			t.Fatalf("Get(%s) from mirrored drive store: mismatch", name)
		}
	}
}
