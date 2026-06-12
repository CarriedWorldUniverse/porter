package status

import (
	"context"
	"testing"
	"time"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
)

func TestGetBackupStatus(t *testing.T) {
	h := NewHolder(6 * time.Hour)
	srv := NewServer(h)

	resp, err := srv.GetBackupStatus(context.Background(), &cwbv1.GetBackupStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus().GetLastAttempt() != nil {
		t.Fatal("empty holder must yield unset last_attempt")
	}

	now := time.Date(2026, 6, 12, 16, 0, 0, 0, time.UTC)
	h.RecordSuccess(now, []Source{{Name: "sqld", SizeBytes: 42}})
	resp, err = srv.GetBackupStatus(context.Background(), &cwbv1.GetBackupStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	st := resp.GetStatus()
	if st.GetLastSuccess().AsTime() != now || len(st.GetSources()) != 1 || st.GetSources()[0].GetName() != "sqld" || st.GetSources()[0].GetSizeBytes() != 42 {
		t.Fatalf("status = %+v", st)
	}
	if st.GetNextDue().AsTime() != now.Add(6*time.Hour) {
		t.Fatalf("next_due = %v", st.GetNextDue().AsTime())
	}

	// Failure pass: last_success is retained, error + attempt + next_due are updated.
	failureTime := now.Add(6 * time.Hour)
	h.RecordFailure(failureTime, "drive: 503")
	resp, err = srv.GetBackupStatus(context.Background(), &cwbv1.GetBackupStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	st = resp.GetStatus()
	if st.GetLastSuccess().AsTime() != now {
		t.Fatalf("last_success after failure = %v, want %v", st.GetLastSuccess().AsTime(), now)
	}
	if st.GetLastError() != "drive: 503" {
		t.Fatalf("last_error = %q, want %q", st.GetLastError(), "drive: 503")
	}
	if st.GetLastAttempt().AsTime() != failureTime {
		t.Fatalf("last_attempt = %v, want %v", st.GetLastAttempt().AsTime(), failureTime)
	}
	if st.GetNextDue().AsTime() != failureTime.Add(6*time.Hour) {
		t.Fatalf("next_due after failure = %v, want %v", st.GetNextDue().AsTime(), failureTime.Add(6*time.Hour))
	}
}
