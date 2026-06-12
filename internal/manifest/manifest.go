// Package manifest defines porter-backup's per-run manifest: the record of
// what one sync pass snapshotted, sealed, and uploaded. The manifest is what
// a restore reads FIRST — it carries each source's plaintext SHA-256 (for
// post-unseal verification), the Drive file id (for download), and the casket
// recipient key ids (which keys can open the blob). Manifests are themselves
// sealed and uploaded, and are never pruned.
package manifest

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// timestampLayout is the run timestamp wire format — compact UTC, safe in
// file names and Drive object names: 20260612T120000Z.
const timestampLayout = "20060102T150405Z"

// SourceEntry records one source's snapshot within a run.
type SourceEntry struct {
	// Name is the source name from the backup config (e.g. "almanac").
	Name string `json:"name"`
	// Artifact is the snapshot's plaintext file name (e.g. "almanac.db",
	// "croft-home.tar.gz") — restore writes the recovered bytes under it.
	Artifact string `json:"artifact"`
	// SHA256 is the lowercase hex SHA-256 of the PLAINTEXT artifact.
	SHA256 string `json:"sha256"`
	// Size is the plaintext artifact size in bytes.
	Size int64 `json:"size"`
	// DriveFileID is the Drive id of the uploaded .casket blob.
	DriveFileID string `json:"drive_file_id"`
	// CasketKeyIDs are the envelope's recipient key ids (lowercase hex,
	// 8 bytes each — casket.RecipientIDs), recorded so restore tooling can
	// say which keys open the blob without downloading it.
	CasketKeyIDs []string `json:"casket_keyids,omitempty"`
}

// Manifest is one sync run's record.
type Manifest struct {
	// Timestamp is the run timestamp in the compact UTC layout
	// (FormatTimestamp); it names the run's objects on Drive.
	Timestamp string        `json:"timestamp"`
	Sources   []SourceEntry `json:"sources"`
}

// Encode renders the manifest as JSON (indented — manifests are small and a
// human reads them during a restore drill).
func (m *Manifest) Encode() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// Decode parses an encoded manifest.
func Decode(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}
	return &m, nil
}

// Entry returns the named source's entry.
func (m *Manifest) Entry(name string) (SourceEntry, bool) {
	for _, e := range m.Sources {
		if e.Name == name {
			return e, true
		}
	}
	return SourceEntry{}, false
}

// VerifySHA256 checks data against a hex SHA-256, constant-time on the digest
// compare and case-insensitive on the hex.
func VerifySHA256(data []byte, wantHex string) error {
	want, err := hex.DecodeString(strings.ToLower(wantHex))
	if err != nil {
		return fmt.Errorf("manifest sha256 is not valid hex: %w", err)
	}
	got := sha256.Sum256(data)
	if len(want) != sha256.Size || subtle.ConstantTimeCompare(got[:], want) != 1 {
		return fmt.Errorf("sha256 mismatch: got %s, manifest says %s",
			hex.EncodeToString(got[:]), strings.ToLower(wantHex))
	}
	return nil
}

// FormatTimestamp renders a run timestamp in the compact UTC layout.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

// ParseTimestamp parses a compact-UTC run timestamp.
func ParseTimestamp(s string) (time.Time, error) {
	t, err := time.Parse(timestampLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing run timestamp %q (want %s): %w", s, timestampLayout, err)
	}
	return t, nil
}

// SnapshotObjectPath is the casket AAD object path AND Drive-relative naming
// convention for one source's sealed snapshot. Seal and restore must agree on
// it byte-for-byte — it is authenticated data.
func SnapshotObjectPath(source, timestamp string) string {
	return fmt.Sprintf("backups/%s/%s.casket", source, timestamp)
}

// ManifestObjectPath is the casket AAD object path for a run's sealed
// manifest.
func ManifestObjectPath(timestamp string) string {
	return fmt.Sprintf("backups/manifests/%s.json.casket", timestamp)
}
