package main

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/porter/internal/manifest"
)

// parseRecipients parses PORTER_RECIPIENTS: comma-separated base64 (std)
// X25519 public keys, 32 bytes each.
func parseRecipients(s string) ([][]byte, error) {
	var out [][]byte
	for i, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			return nil, fmt.Errorf("recipient %d: not valid base64: %w", i, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("recipient %d: want 32-byte X25519 public key, got %d bytes", i, len(key))
		}
		out = append(out, key)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no recipients configured (PORTER_RECIPIENTS)")
	}
	return out, nil
}

// snapshotDriveName is the Drive file name of one source snapshot.
func snapshotDriveName(timestamp string) string { return timestamp + ".casket" }

// manifestDriveName is the Drive file name of one run manifest.
func manifestDriveName(timestamp string) string { return timestamp + ".json.casket" }

// parseSnapshotName recovers a snapshot's run time from its Drive file name.
// Non-snapshot names (anything that isn't <timestamp>.casket) return ok=false
// — retention must NEVER delete files it does not recognize.
func parseSnapshotName(name string) (time.Time, bool) {
	ts, found := strings.CutSuffix(name, ".casket")
	if !found || strings.Contains(ts, ".") {
		return time.Time{}, false
	}
	t, err := manifest.ParseTimestamp(ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
