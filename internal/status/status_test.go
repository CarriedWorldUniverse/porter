package status

import (
	"testing"
	"time"
)

func TestHolderRecordsPasses(t *testing.T) {
	h := NewHolder(6 * time.Hour)
	if got := h.Get(); got.LastAttempt != nil {
		t.Fatal("fresh holder must be empty")
	}

	now := time.Date(2026, 6, 12, 16, 0, 0, 0, time.UTC)
	h.RecordSuccess(now, []Source{{Name: "sqld", SizeBytes: 7868657}})
	got := h.Get()
	if got.LastSuccess == nil || !got.LastSuccess.Equal(now) {
		t.Fatalf("LastSuccess = %v", got.LastSuccess)
	}
	if got.LastError != "" || len(got.Sources) != 1 || got.Sources[0].Name != "sqld" {
		t.Fatalf("got %+v", got)
	}
	if want := now.Add(6 * time.Hour); !got.NextDue.Equal(want) {
		t.Fatalf("NextDue = %v want %v", got.NextDue, want)
	}

	later := now.Add(6 * time.Hour)
	h.RecordFailure(later, "drive: 503")
	got = h.Get()
	if got.LastError != "drive: 503" {
		t.Fatalf("LastError = %q", got.LastError)
	}
	if !got.LastSuccess.Equal(now) {
		t.Fatal("failure must not clobber LastSuccess")
	}
	if !got.LastAttempt.Equal(later) {
		t.Fatal("LastAttempt must advance on failure")
	}
	if len(got.Sources) != 1 {
		t.Fatal("failure must not clobber last good sources")
	}
}
