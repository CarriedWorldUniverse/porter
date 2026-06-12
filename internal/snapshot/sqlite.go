package snapshot

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	// Pure-Go sqlite driver (no cgo) — the binary stays CGO_ENABLED=0 /
	// scratch-image friendly.
	_ "modernc.org/sqlite"
)

// snapshotSQLite produces a consistent copy of a live sqlite database at src
// into dst using `VACUUM INTO`. VACUUM INTO runs inside a read transaction,
// so under WAL it neither blocks nor is blocked by concurrent writers — the
// copy is a consistent point-in-time image and the service is never paused.
// The source is opened read-only (mode=ro), so a typo'd path can never create
// or mutate a database.
func snapshotSQLite(ctx context.Context, src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("sqlite source: %w", err)
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("sqlite source: %w", err)
	}
	// busy_timeout guards the brief internal locks sqlite still takes; the
	// url.URL form percent-encodes spaces/odd chars correctly for file: URIs.
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&_pragma=busy_timeout(10000)"}
	dsn := u.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("opening sqlite source %s: %w", src, err)
	}
	defer db.Close()
	// VACUUM INTO refuses an existing destination — that is what we want
	// (snapshots are new files in a fresh work dir).
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("VACUUM INTO for %s: %w", src, err)
	}
	return nil
}
