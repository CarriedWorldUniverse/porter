package retention

import (
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// now for all tests: 2026-06-12 12:00 UTC. The 30-day boundary is
// 2026-05-13 12:00 UTC.
var now = ts("2026-06-12T12:00:00Z")

func ids(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func assertIDs(t *testing.T, got []Item, want ...string) {
	t.Helper()
	gotIDs := ids(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("got %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("got %v, want %v", gotIDs, want)
		}
	}
}

func TestToDelete_AllRecentKept(t *testing.T) {
	items := []Item{
		{ID: "a", Time: ts("2026-06-12T06:00:00Z")},
		{ID: "b", Time: ts("2026-06-01T00:00:00Z")},
		{ID: "c", Time: ts("2026-05-20T00:00:00Z")},
	}
	assertIDs(t, ToDelete(items, now))
}

func TestToDelete_OldDeletedExceptMonthlyKeeper(t *testing.T) {
	items := []Item{
		// March: two snapshots — earliest is the monthly keeper.
		{ID: "mar-early", Time: ts("2026-03-02T01:00:00Z")},
		{ID: "mar-late", Time: ts("2026-03-20T01:00:00Z")},
		// April: one snapshot — it is its month's keeper.
		{ID: "apr-only", Time: ts("2026-04-15T00:00:00Z")},
		// May, older than 30d: keeper + one extra.
		{ID: "may-early", Time: ts("2026-05-01T00:00:00Z")},
		{ID: "may-mid", Time: ts("2026-05-10T00:00:00Z")},
		// Recent: kept by age regardless.
		{ID: "jun", Time: ts("2026-06-10T00:00:00Z")},
	}
	assertIDs(t, ToDelete(items, now), "mar-late", "may-mid")
}

func TestToDelete_ExactBoundaryKept(t *testing.T) {
	// Exactly 30 days old is NOT "older than 30 days" — kept.
	items := []Item{
		{ID: "boundary", Time: ts("2026-05-13T12:00:00Z")},
		// One second past the boundary, and not a monthly keeper (an
		// earlier May snapshot exists).
		{ID: "past", Time: ts("2026-05-13T11:59:59Z")},
		{ID: "may-keeper", Time: ts("2026-05-03T00:00:00Z")},
	}
	assertIDs(t, ToDelete(items, now), "past")
}

func TestToDelete_KeeperIsEarliestEvenIfListUnsorted(t *testing.T) {
	items := []Item{
		{ID: "mar-late", Time: ts("2026-03-20T00:00:00Z")},
		{ID: "mar-mid", Time: ts("2026-03-10T00:00:00Z")},
		{ID: "mar-early", Time: ts("2026-03-05T00:00:00Z")},
	}
	assertIDs(t, ToDelete(items, now), "mar-late", "mar-mid")
}

func TestToDelete_MonthBucketsRespectYear(t *testing.T) {
	// March 2025 and March 2026 are different buckets — each keeps its own
	// earliest.
	items := []Item{
		{ID: "mar25-a", Time: ts("2025-03-01T00:00:00Z")},
		{ID: "mar25-b", Time: ts("2025-03-15T00:00:00Z")},
		{ID: "mar26-a", Time: ts("2026-03-01T00:00:00Z")},
		{ID: "mar26-b", Time: ts("2026-03-15T00:00:00Z")},
	}
	assertIDs(t, ToDelete(items, now), "mar25-b", "mar26-b")
}

func TestToDelete_MonthlyKeeperUsesUTC(t *testing.T) {
	// 2026-04-01T00:30+10:00 is 2026-03-31T14:30 UTC — a MARCH snapshot.
	// It is earlier (in absolute time) than the pure-UTC March keeper
	// candidate below, so it becomes March's keeper.
	loc := time.FixedZone("AEST", 10*3600)
	items := []Item{
		{ID: "tz", Time: time.Date(2026, 4, 1, 0, 30, 0, 0, loc)},
		{ID: "mar-utc", Time: ts("2026-03-31T20:00:00Z")},
		{ID: "apr-utc", Time: ts("2026-04-02T00:00:00Z")},
	}
	assertIDs(t, ToDelete(items, now), "mar-utc")
}

func TestToDelete_Empty(t *testing.T) {
	if got := ToDelete(nil, now); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}
