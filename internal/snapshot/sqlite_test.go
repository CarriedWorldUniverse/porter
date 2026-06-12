package snapshot

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newFixtureDB creates a WAL-mode sqlite db with one table and n seed rows.
func newFixtureDB(t *testing.T, n int) (path string, db *sql.DB) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("WAL: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Exec("INSERT INTO kv (v) VALUES (?)", fmt.Sprintf("row-%d", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return path, db
}

func openAndCheck(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	var res string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if res != "ok" {
		t.Fatalf("integrity_check: %q", res)
	}
	return db
}

func TestSnapshotSQLite(t *testing.T) {
	src, _ := newFixtureDB(t, 10)
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := snapshotSQLite(context.Background(), src, dst); err != nil {
		t.Fatalf("snapshotSQLite: %v", err)
	}
	db := openAndCheck(t, dst)
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM kv").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 10 {
		t.Fatalf("rows: got %d want 10", n)
	}
}

// TestSnapshotSQLiteLiveWriter proves the WAL-safe consistent-copy claim: a
// writer keeps inserting (its own connection, WAL mode) while the snapshot
// runs. The copy must pass integrity_check and contain a consistent row count
// — at least the rows committed before the snapshot started, with no torn
// state — and the live db must never be paused (the writer keeps committing
// throughout).
func TestSnapshotSQLiteLiveWriter(t *testing.T) {
	src, db := newFixtureDB(t, 100)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	writes, writeErrs := 0, 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_, err := db.Exec("INSERT INTO kv (v) VALUES (?)", fmt.Sprintf("live-%d", i))
			mu.Lock()
			if err != nil {
				writeErrs++
			} else {
				writes++
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	// Let the writer get going, then snapshot under live writes.
	time.Sleep(50 * time.Millisecond)
	dst := filepath.Join(t.TempDir(), "snap.db")
	err := snapshotSQLite(context.Background(), src, dst)
	cancel()
	wg.Wait()
	if err != nil {
		t.Fatalf("snapshotSQLite under live writes: %v", err)
	}

	mu.Lock()
	w, we := writes, writeErrs
	mu.Unlock()
	if w == 0 {
		t.Fatal("live writer made no writes — test proves nothing")
	}
	if we > 0 {
		t.Fatalf("live writer hit %d errors — snapshot paused/locked the db", we)
	}

	snap := openAndCheck(t, dst)
	var n int
	if err := snap.QueryRow("SELECT COUNT(*) FROM kv").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n < 100 {
		t.Fatalf("snapshot lost pre-existing rows: got %d want >= 100", n)
	}
	// And the LIVE db kept all its rows (snapshot was non-destructive).
	var liveN int
	if err := db.QueryRow("SELECT COUNT(*) FROM kv").Scan(&liveN); err != nil {
		t.Fatalf("live count: %v", err)
	}
	if liveN < 100+w {
		t.Fatalf("live db rows: got %d want >= %d", liveN, 100+w)
	}
}

func TestSnapshotSQLiteMissingSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := snapshotSQLite(context.Background(), filepath.Join(t.TempDir(), "absent.db"), dst); err == nil {
		t.Fatal("want error for missing source db")
	}
}
