// Package packstore is porter's experimental log-structured encrypted
// object store: a place a cloud provider can host that sees only uniform
// opaque ciphertext blocks. Fixed-size padded "packs" hold content-addressed
// chunks of artifacts; a generation-versioned encrypted index and a
// write-last "superblock" describe which packs and chunks make up which
// artifacts. Crash consistency comes entirely from write ORDER — packs,
// then the new index, then the new superblock — so a crash mid-commit
// leaves at most harmless orphans (unreferenced packs/indexes), never a
// corrupt-looking store: the latest fully-written superblock is always
// self-consistent.
//
// Every object (pack, index, superblock) is sealed with
// internal/envelope.Seal before it ever reaches a Backend, so the Backend
// itself only ever stores ciphertext.
package packstore

import "errors"

// ErrExists is the sentinel a Backend's Put must wrap (fmt.Errorf("...: %w",
// ErrExists)) when it refuses to overwrite an existing object. Callers that
// need to distinguish "already there" from a genuine failure — e.g. Mirror,
// racing another writer/mirror on the same name — check errors.Is(err,
// ErrExists).
var ErrExists = errors.New("packstore: object already exists")

// Backend is the write-once object store packstore runs on top of. Object
// names are opaque strings chosen by packstore; a Backend must not inspect
// or interpret them beyond byte-for-byte storage, lookup, and prefix
// listing.
type Backend interface {
	// Put writes name's contents. It MUST fail, wrapping ErrExists, if name
	// already exists — packstore relies on write-once semantics for crash
	// consistency.
	Put(name string, data []byte) error
	// Get reads name's full contents.
	Get(name string) ([]byte, error)
	// List returns the lexicographically sorted names of all objects
	// whose name starts with prefix.
	List(prefix string) ([]string, error)
	// Delete removes name. packstore does not currently call this itself
	// (garbage collection is a future unit); it exists so a Backend
	// implementation has a complete write-once-object-store surface.
	Delete(name string) error
}
