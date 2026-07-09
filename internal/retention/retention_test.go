package retention

import (
	"sort"
	"testing"
	"time"
)

// ids returns the sorted ID set of a slice, for order-independent comparison.
func ids(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRetentionPolicy(t *testing.T) {
	// Fixed "now" on a Wednesday so week boundaries are unambiguous.
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC) // Wed
	day := 24 * time.Hour

	at := func(id string, ago time.Duration) Item { return Item{ID: id, Time: now.Add(-ago)} }

	items := []Item{
		at("now", 0),
		at("d3", 3*day), // within 7d → keep
		at("d7", 7*day), // exactly 7d: aged == window → within (not before cutoff) → keep
		// same ISO week (Mon Jun 22 – Sun Jun 28), both >7d: only earliest kept
		at("w2_early", 12*day), // Fri Jun 26
		at("w2_late", 10*day),  // Sun Jun 28 (same week) → deleted (not the keeper)
		// a different older week (Jun 15–21): its sole snapshot is the keeper
		at("w3", 18*day), // Sat Jun 20
		// beyond the 1-month horizon (>30d): deleted even though it'd be a weekly keeper
		at("old1", 35*day),
		at("old2", 40*day),
	}

	del := ids(ToDelete(items, now))
	// Deleted: w2_late (not the weekly keeper of its week) + old1 + old2 (past horizon).
	want := []string{"old1", "old2", "w2_late"}
	if !eq(del, want) {
		t.Fatalf("ToDelete = %v, want %v", del, want)
	}
}

func TestRollingWindowKeepsAllRecent(t *testing.T) {
	now := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	var items []Item
	// 28 snapshots at 6h over the last 7 days — all inside the window, none deleted.
	for i := 0; i < 28; i++ {
		items = append(items, Item{ID: string(rune('a' + i)), Time: now.Add(-time.Duration(i) * 6 * time.Hour)})
	}
	if got := ToDelete(items, now); len(got) != 0 {
		t.Fatalf("nothing within 7d should be deleted, got %d", len(got))
	}
}

func TestHorizonCapDropsOldWeeklyKeepers(t *testing.T) {
	now := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	// A lone snapshot 45 days old is the sole (thus earliest = keeper) of its
	// week, but past the horizon → must be deleted.
	items := []Item{{ID: "ancient", Time: now.Add(-45 * day)}}
	got := ToDelete(items, now)
	if len(got) != 1 || got[0].ID != "ancient" {
		t.Fatalf("past-horizon lone-week snapshot must be deleted, got %v", ids(got))
	}
}

func TestEmptyAndSingle(t *testing.T) {
	now := time.Now().UTC()
	if got := ToDelete(nil, now); len(got) != 0 {
		t.Fatalf("nil → nothing, got %d", len(got))
	}
	if got := ToDelete([]Item{{ID: "x", Time: now}}, now); len(got) != 0 {
		t.Fatalf("single recent → keep, got %d", len(got))
	}
}
