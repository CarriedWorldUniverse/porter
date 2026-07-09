package packstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/CarriedWorldUniverse/porter/internal/envelope"
)

// List returns the names of every artifact currently in the store, sorted.
func (r *Reader) List() []string {
	out := make([]string, 0, len(r.idx.Names))
	for name := range r.idx.Names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Get reassembles an artifact's plaintext bytes, verifying every chunk's
// sha256 against its content id. It returns an error rather than corrupt
// data if any chunk fails its AEAD open or its hash check.
func (r *Reader) Get(name string) ([]byte, error) {
	entry, ok := r.idx.Names[name]
	if !ok {
		return nil, fmt.Errorf("packstore: get %s: artifact not found", name)
	}
	if len(entry.Chunks) == 0 {
		return []byte{}, nil
	}

	out := make([]byte, 0, entry.Size)
	for _, id := range entry.Chunks {
		loc, ok := r.idx.Chunks[id]
		if !ok {
			return nil, fmt.Errorf("packstore: get %s: chunk %s missing from index", name, id)
		}
		packData, err := r.getPack(loc.Pack)
		if err != nil {
			return nil, fmt.Errorf("packstore: get %s: %w", name, err)
		}
		if loc.Offset < 0 || loc.Length < 0 || loc.Offset+loc.Length > len(packData) {
			return nil, fmt.Errorf("packstore: get %s: chunk %s location out of range in pack %s", name, id, loc.Pack)
		}
		chunk := packData[loc.Offset : loc.Offset+loc.Length]
		sum := sha256.Sum256(chunk)
		if hex.EncodeToString(sum[:]) != id {
			return nil, fmt.Errorf("packstore: get %s: chunk %s failed hash verification (tampered pack %s?)", name, id, loc.Pack)
		}
		out = append(out, chunk...)
	}
	if int64(len(out)) != entry.Size {
		return nil, fmt.Errorf("packstore: get %s: reassembled %d bytes, index says %d", name, len(out), entry.Size)
	}
	return out, nil
}

// getPack fetches and unseals a pack, caching the plaintext by pack name.
func (r *Reader) getPack(name string) ([]byte, error) {
	if cached, ok := r.packCache[name]; ok {
		return cached, nil
	}
	sealed, err := r.b.Get(name)
	if err != nil {
		return nil, fmt.Errorf("reading pack %s: %w", name, err)
	}
	plain, err := envelope.Unseal(r.privKey, sealed, objectPath(r.storeID, name))
	if err != nil {
		return nil, fmt.Errorf("unsealing pack %s: %w", name, err)
	}
	if r.packCache == nil {
		r.packCache = map[string][]byte{}
	}
	r.packCache[name] = plain
	return plain, nil
}
