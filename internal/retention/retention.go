// Package retention implements porter-backup's prune policy as a pure
// function. Policy (operator, 2026-07-09): keep every snapshot from the last 7
// days; older than that keep one snapshot per UTC ISO-week (the "weekly
// keeper") out to a one-month horizon; delete everything older than the
// horizon — INCLUDING weekly keepers (the horizon is a hard cap, unlike the
// old monthly-keeper-forever policy). Manifests are never passed through this
// policy — they are never pruned (the spec keeps all manifests).
package retention

import "time"

const (
	// RollingWindow keeps every snapshot aged <= this (daily granularity in
	// practice, since backups run more often than daily).
	RollingWindow = 7 * 24 * time.Hour
	// KeeperHorizon is the hard outer bound: nothing older than this is kept,
	// weekly keepers included. "A month" of weekly keepers behind the window.
	KeeperHorizon = 30 * 24 * time.Hour
)

// Item is one stored snapshot of a single source: an opaque identifier (e.g.
// the Drive file id) and the snapshot's timestamp.
type Item struct {
	ID   string
	Time time.Time
}

// ToDelete returns the items the retention policy says to delete, given one
// source's snapshot list and the current time. Order of the returned items
// follows the input order. The input may be unsorted.
//
// Policy:
//   - aged <= RollingWindow (7d)           → keep (all of them);
//   - RollingWindow < aged <= KeeperHorizon → keep iff it is the earliest
//     snapshot of its UTC ISO-week (the weekly keeper); else delete;
//   - aged > KeeperHorizon (1 month)        → delete, weekly keepers included.
func ToDelete(items []Item, now time.Time) []Item {
	rollingCutoff := now.Add(-RollingWindow)
	horizonCutoff := now.Add(-KeeperHorizon)

	// Earliest item per UTC ISO-week bucket across ALL items — the weekly
	// keeper. Computing keepers over all items (not just old ones) is
	// deliberate: the earliest snapshot of a week stays the keeper as it ages
	// past the rolling window.
	type week struct {
		isoYear int
		isoWeek int
	}
	earliest := make(map[week]int) // bucket -> index of earliest item
	for i, it := range items {
		y, w := it.Time.UTC().ISOWeek()
		b := week{y, w}
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
		if !it.Time.Before(rollingCutoff) { // within 7d: keep
			continue
		}
		if it.Time.Before(horizonCutoff) { // older than the horizon: delete, keeper or not
			out = append(out, it)
			continue
		}
		if keeper[i] { // 7d..1mo: weekly keeper survives
			continue
		}
		out = append(out, it)
	}
	return out
}
