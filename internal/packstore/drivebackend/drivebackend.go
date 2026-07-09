// Package drivebackend is a packstore.Backend backed by Google Drive: every
// object for one store lives flat in a single Drive folder, addressed by
// Drive file id. packstore.Backend has no per-call context, so a Backend
// stores the context it was constructed with and reuses it for every Drive
// call — callers that need per-request cancellation should construct a new
// Backend for that request instead.
//
// packstore's write-once contract is a single-writer contract: Put refusing
// an existing name is enforced by a name->id cache check (refreshed from
// Drive on a cache miss), not by any Drive-side compare-and-swap. Concurrent
// writers racing on the same name can both pass the check and both upload —
// that is out of scope here, matching packstore/localdir's own "no
// concurrency control beyond a single writer" stance.
package drivebackend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/packstore"
)

var _ packstore.Backend = (*Backend)(nil)

// Backend is a packstore.Backend backed by one Drive folder.
type Backend struct {
	ctx      context.Context
	c        *drive.Client
	folderID string

	mu     sync.Mutex
	byName map[string]string // name -> Drive file id
}

// New wraps an existing *drive.Client and a folder id as a packstore
// Backend. All objects for one store live flat in that one folder. ctx is
// stored and reused for every Drive call made through the returned Backend
// (see the package doc).
func New(ctx context.Context, c *drive.Client, folderID string) *Backend {
	return &Backend{
		ctx:      ctx,
		c:        c,
		folderID: folderID,
		byName:   map[string]string{},
	}
}

// refreshLocked repopulates the name->id cache from a full folder List.
// Caller must hold b.mu.
func (b *Backend) refreshLocked() error {
	files, err := b.c.List(b.ctx, b.folderID)
	if err != nil {
		return fmt.Errorf("drivebackend: listing folder: %w", err)
	}
	fresh := make(map[string]string, len(files))
	for _, f := range files {
		fresh[f.Name] = f.ID
	}
	b.byName = fresh
	return nil
}

// resolveLocked returns name's Drive file id, refreshing the cache once on a
// miss. Caller must hold b.mu.
func (b *Backend) resolveLocked(name string) (string, bool, error) {
	if id, ok := b.byName[name]; ok {
		return id, true, nil
	}
	if err := b.refreshLocked(); err != nil {
		return "", false, err
	}
	id, ok := b.byName[name]
	return id, ok, nil
}

// Put writes name's contents. It refuses if name already exists (consulting
// the name->id cache, refreshed from Drive once on a miss — see the package
// doc for the single-writer caveat this relies on).
func (b *Backend) Put(name string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok, err := b.resolveLocked(name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("drivebackend: put %s: already exists", name)
	}

	id, err := b.c.Upload(b.ctx, name, b.folderID, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("drivebackend: put %s: %w", name, err)
	}
	b.byName[name] = id
	return nil
}

// Get reads name's full contents.
func (b *Backend) Get(name string) ([]byte, error) {
	b.mu.Lock()
	id, ok, err := b.resolveLocked(name)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("drivebackend: get %s: not found", name)
	}

	rc, err := b.c.Download(b.ctx, id)
	if err != nil {
		return nil, fmt.Errorf("drivebackend: get %s: %w", name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("drivebackend: get %s: reading: %w", name, err)
	}
	return data, nil
}

// List returns the lexicographically sorted names of all objects whose name
// starts with prefix. It always does a full folder List against Drive and
// refreshes the name->id cache from that same call.
func (b *Backend) List(prefix string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.refreshLocked(); err != nil {
		return nil, fmt.Errorf("drivebackend: list %q: %w", prefix, err)
	}
	var out []string
	for name := range b.byName {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes name.
func (b *Backend) Delete(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	id, ok, err := b.resolveLocked(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("drivebackend: delete %s: not found", name)
	}
	if err := b.c.Delete(b.ctx, id); err != nil {
		return fmt.Errorf("drivebackend: delete %s: %w", name, err)
	}
	delete(b.byName, name)
	return nil
}
