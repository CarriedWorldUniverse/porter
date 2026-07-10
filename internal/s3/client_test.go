package s3_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/CarriedWorldUniverse/porter/internal/s3"
	"github.com/CarriedWorldUniverse/porter/internal/s3/s3test"
)

func testCreds(url string) s3.Credentials {
	return s3.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "test-secret-access-key",
		Endpoint:        url,
		Bucket:          "test-bucket",
		Region:          "auto",
	}
}

func TestClientCRUD(t *testing.T) {
	creds := testCreds("")
	fake := s3test.New("test-bucket", creds)
	defer fake.Close()
	creds.Endpoint = fake.URL()

	c := s3.New(creds, nil)
	ctx := context.Background()

	if err := c.PutObject(ctx, "pack-a", []byte("AAAA"), true); err != nil {
		t.Fatalf("PutObject(pack-a): %v", err)
	}
	got, err := c.GetObject(ctx, "pack-a")
	if err != nil {
		t.Fatalf("GetObject(pack-a): %v", err)
	}
	if string(got) != "AAAA" {
		t.Fatalf("GetObject(pack-a): got %q", got)
	}

	// Conditional write against an existing key -> ErrPreconditionFailed.
	err = c.PutObject(ctx, "pack-a", []byte("overwrite"), true)
	if !errors.Is(err, s3.ErrPreconditionFailed) {
		t.Fatalf("PutObject(pack-a) again: got %v, want ErrPreconditionFailed", err)
	}
	// Original content untouched.
	got, err = c.GetObject(ctx, "pack-a")
	if err != nil || string(got) != "AAAA" {
		t.Fatalf("GetObject(pack-a) after failed overwrite: %q, %v", got, err)
	}

	// Non-conditional write does overwrite.
	if err := c.PutObject(ctx, "pack-a", []byte("BBBB"), false); err != nil {
		t.Fatalf("PutObject(pack-a) non-conditional: %v", err)
	}
	got, err = c.GetObject(ctx, "pack-a")
	if err != nil || string(got) != "BBBB" {
		t.Fatalf("GetObject(pack-a) after non-conditional overwrite: %q, %v", got, err)
	}

	// Missing key -> ErrNotFound.
	_, err = c.GetObject(ctx, "nope")
	if !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("GetObject(nope): got %v, want ErrNotFound", err)
	}

	// Delete is idempotent.
	if err := c.DeleteObject(ctx, "pack-a"); err != nil {
		t.Fatalf("DeleteObject(pack-a): %v", err)
	}
	if err := c.DeleteObject(ctx, "pack-a"); err != nil {
		t.Fatalf("DeleteObject(pack-a) again (already gone): %v", err)
	}
	_, err = c.GetObject(ctx, "pack-a")
	if !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("GetObject(pack-a) after delete: got %v, want ErrNotFound", err)
	}
}

func TestClientListObjectsPagination(t *testing.T) {
	creds := testCreds("")
	fake := s3test.New("test-bucket", creds)
	defer fake.Close()
	creds.Endpoint = fake.URL()
	fake.SetPageSize(2) // force pagination

	c := s3.New(creds, nil)
	ctx := context.Background()

	const n = 9 // > 2 pages at page size 2
	var want []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("prefix/obj-%02d", i)
		if err := c.PutObject(ctx, name, []byte("x"), true); err != nil {
			t.Fatalf("PutObject(%s): %v", name, err)
		}
		want = append(want, name)
	}
	// A sibling outside the prefix must not show up.
	if err := c.PutObject(ctx, "other/obj", []byte("y"), true); err != nil {
		t.Fatal(err)
	}

	got, err := c.ListObjects(ctx, "prefix/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ListObjects: got %d keys %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListObjects[%d]: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestClientListObjectsEmptyPrefix(t *testing.T) {
	creds := testCreds("")
	fake := s3test.New("test-bucket", creds)
	defer fake.Close()
	creds.Endpoint = fake.URL()

	c := s3.New(creds, nil)
	ctx := context.Background()
	got, err := c.ListObjects(ctx, "")
	if err != nil {
		t.Fatalf("ListObjects(\"\") on empty bucket: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListObjects(\"\") on empty bucket: got %v, want empty", got)
	}
}
