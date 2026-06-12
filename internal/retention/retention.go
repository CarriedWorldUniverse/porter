// Package retention implements porter-backup's prune policy as a pure
// function: snapshots older than 30 days are deleted, EXCEPT the earliest
// snapshot of each UTC calendar month (the "monthly keeper"), which is kept
// forever. Manifests are never passed through this policy — they are never
// pruned (the spec keeps all manifests).
package retention

import "time"

// MaxAge is the rolling retention window: snapshots strictly older than this
// are prune candidates (monthly keepers excepted).
const MaxAge = 30 * 24 * time.Hour

// Item is one stored snapshot of a single source: an opaque identifier (e.g.
// the Drive file id) and the snapshot's timestamp.
type Item struct {
	ID   string
	Time time.Time
}

// ToDelete returns the items that the retention policy says to delete, given
// one source's snapshot list and the current time. Order of the returned
// items follows the input order. The input may be unsorted.
//
// Policy:
//   - items aged <= MaxAge (relative to now) are kept;
//   - of the items strictly older than MaxAge, the earliest item of each UTC
//     calendar month is kept (first-of-month keeper);
//   - everything else older than MaxAge is deleted.
func ToDelete(items []Item, now time.Time) []Item {
	cutoff := now.Add(-MaxAge)

	// Earliest item per UTC (year, month) bucket across ALL items — the
	// monthly keeper. Computing keepers over all items (not just the old
	// ones) is deliberate: the earliest snapshot of a month stays the
	// keeper as it ages past the cutoff.
	type bucket struct {
		year  int
		month time.Month
	}
	earliest := make(map[bucket]int) // bucket -> index of earliest item
	for i, it := range items {
		u := it.Time.UTC()
		b := bucket{u.Year(), u.Month()}
		j, ok := earliest[b]
		if !ok || it.Time.Before(items[j].Time) {
			earliest[b] = i
		}
	}
	keeper := make(map[int]bool, len(earliest))
	for _, i := range earliest {
		keeper[i] = true
	}

	var out []Item
	for i, it := range items {
		if !it.Time.Before(cutoff) { // aged <= MaxAge: kept
			continue
		}
		if keeper[i] {
			continue
		}
		out = append(out, it)
	}
	return out
}
