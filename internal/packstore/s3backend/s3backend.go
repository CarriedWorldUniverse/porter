// Package s3backend is a packstore.Backend backed by an S3-compatible
// bucket (Amazon S3, or Cloudflare R2) via internal/s3.Client. All objects
// for one store live under one key prefix in one bucket.
//
// Unlike drivebackend (whose write-once refusal is only a client-side
// cache check, racy across writers), Put here relies on the underlying
// service's real conditional PUT (If-None-Match: "*") — the bucket itself
// performs the compare-and-swap, so two writers or mirrors racing on the
// same name are safe: exactly one Put succeeds and the other gets
// packstore.ErrExists, with no cache to go stale.
package s3backend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/s3"
)

var _ packstore.Backend = (*Backend)(nil)

// Backend is a packstore.Backend backed by one S3-compatible bucket, under
// one key prefix. packstore.Backend has no per-call context, so a Backend
// stores the context it was constructed with and reuses it for every S3
// call — callers that need per-request cancellation should construct a new
// Backend for that request instead.
type Backend struct {
	ctx    context.Context
	c      *s3.Client
	prefix string // trailing slash trimmed; "" means the bucket root
}

// New wraps an existing *s3.Client as a packstore Backend. prefix scopes
// every object under "<prefix>/<name>" (a trailing slash on prefix is
// trimmed; an empty prefix stores objects directly at the bucket root).
// ctx is stored and reused for every S3 call made through the returned
// Backend (see the package doc).
func New(ctx context.Context, c *s3.Client, prefix string) *Backend {
	return &Backend{ctx: ctx, c: c, prefix: strings.TrimRight(prefix, "/")}
}

// fullKey maps a packstore object name to its full S3 key under prefix.
func (b *Backend) fullKey(name string) string {
	if b.prefix == "" {
		return name
	}
	return b.prefix + "/" + name
}

// Put writes name's contents with a conditional (If-None-Match: "*") PUT,
// wrapping a precondition-failed response as packstore.ErrExists.
func (b *Backend) Put(name string, data []byte) error {
	if err := b.c.PutObject(b.ctx, b.fullKey(name), data, true); err != nil {
		if errors.Is(err, s3.ErrPreconditionFailed) {
			return fmt.Errorf("s3backend: put %s: %w", name, packstore.ErrExists)
		}
		return fmt.Errorf("s3backend: put %s: %w", name, err)
	}
	return nil
}

// Get reads name's full contents.
func (b *Backend) Get(name string) ([]byte, error) {
	data, err := b.c.GetObject(b.ctx, b.fullKey(name))
	if err != nil {
		return nil, fmt.Errorf("s3backend: get %s: %w", name, err)
	}
	return data, nil
}

// List returns the lexicographically sorted names of all objects whose
// name starts with prefix (packstore's name prefix, not the Backend's key
// prefix — the Backend's own prefix is stripped back off before names are
// returned).
func (b *Backend) List(prefix string) ([]string, error) {
	keys, err := b.c.ListObjects(b.ctx, b.fullKey(prefix))
	if err != nil {
		return nil, fmt.Errorf("s3backend: list %q: %w", prefix, err)
	}
	strip := b.prefix
	if strip != "" {
		strip += "/"
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strings.TrimPrefix(k, strip))
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes name.
func (b *Backend) Delete(name string) error {
	if err := b.c.DeleteObject(b.ctx, b.fullKey(name)); err != nil {
		return fmt.Errorf("s3backend: delete %s: %w", name, err)
	}
	return nil
}
