package packstore

import (
	"errors"
	"fmt"
	"sort"
)

// Mirror copies every object in src that dst does not already have. It
// carries ciphertext only (packstore.Backend never sees plaintext) and
// requires no keys — it never decrypts, and it never needs to, since a
// mirror is just a byte-for-byte copy of a write-once object store onto
// another Backend.
//
// Objects are copied in a strict order — all "pack-" objects first, then
// all "idx-" objects, then any objects of an unrecognized kind, then all
// "sb-" objects last — lexicographically within each group. This makes dst
// a consistent, openable store at every point during (and after any
// interruption of) a mirror pass: a superblock never lands before the
// index and packs it references. Because packstore objects are write-once
// and sb- always lands last, Mirror is safe to run concurrently with a
// writer appending new generations to src (the mirror simply may or may
// not pick up the writer's newest generation, depending on timing).
//
// Mirror never calls Delete — it is purely additive (packstore has no
// garbage collection yet).
func Mirror(src, dst Backend) (copied, skipped int, err error) {
	names, err := src.List("")
	if err != nil {
		return 0, 0, fmt.Errorf("packstore: mirror: listing source: %w", err)
	}
	existing, err := dst.List("")
	if err != nil {
		return 0, 0, fmt.Errorf("packstore: mirror: listing destination: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, name := range existing {
		have[name] = true
	}

	ordered := make([]string, len(names))
	copy(ordered, names)
	sort.SliceStable(ordered, func(i, j int) bool {
		ki, kj := mirrorKindRank(ordered[i]), mirrorKindRank(ordered[j])
		if ki != kj {
			return ki < kj
		}
		return ordered[i] < ordered[j]
	})

	for _, name := range ordered {
		if have[name] {
			skipped++
			continue
		}
		data, err := src.Get(name)
		if err != nil {
			return copied, skipped, fmt.Errorf("packstore: mirror: reading %s from source: %w", name, err)
		}
		if err := dst.Put(name, data); err != nil {
			if errors.Is(err, ErrExists) {
				// Raced with another mirror or writer landing the same
				// object between our List and this Put: treat it as
				// already present, not fatal.
				skipped++
				continue
			}
			return copied, skipped, fmt.Errorf("packstore: mirror: writing %s to destination: %w", name, err)
		}
		copied++
	}
	return copied, skipped, nil
}

// mirrorKindRank orders object kinds for Mirror: pack- first, then idx-,
// then any unrecognized kind, then sb- last.
func mirrorKindRank(name string) int {
	switch {
	case len(name) >= 5 && name[:5] == "pack-":
		return 0
	case len(name) >= 4 && name[:4] == "idx-":
		return 1
	case len(name) >= 3 && name[:3] == "sb-":
		return 3
	default:
		return 2
	}
}
