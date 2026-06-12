package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func sample() *Manifest {
	return &Manifest{
		Timestamp: "20260612T120000Z",
		Sources: []SourceEntry{
			{
				Name:         "almanac",
				Artifact:     "almanac.db",
				SHA256:       strings.Repeat("ab", 32),
				Size:         57344,
				DriveFileID:  "drv-1",
				CasketKeyIDs: []string{"0102030405060708", "1112131415161718"},
			},
			{
				Name:        "croft-home",
				Artifact:    "croft-home.tar.gz",
				SHA256:      strings.Repeat("cd", 32),
				Size:        1500000,
				DriveFileID: "drv-2",
			},
		},
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	m := sample()
	data, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Timestamp != m.Timestamp {
		t.Fatalf("timestamp: got %q want %q", got.Timestamp, m.Timestamp)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("sources: got %d want 2", len(got.Sources))
	}
	if got.Sources[0].Name != m.Sources[0].Name || got.Sources[0].SHA256 != m.Sources[0].SHA256 || got.Sources[0].Artifact != "almanac.db" {
		t.Fatalf("source[0] mismatch: %+v", got.Sources[0])
	}
	if got.Sources[0].DriveFileID != "drv-1" || got.Sources[0].Size != 57344 {
		t.Fatalf("source[0] fields lost: %+v", got.Sources[0])
	}
	if len(got.Sources[0].CasketKeyIDs) != 2 {
		t.Fatalf("keyids lost: %+v", got.Sources[0])
	}
}

func TestJSONFieldNames(t *testing.T) {
	// The manifest wire shape is part of the restore contract — pin the
	// field names.
	data, err := sample().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		`"timestamp"`, `"sources"`, `"name"`, `"sha256"`, `"size"`,
		`"drive_file_id"`, `"casket_keyids"`, `"artifact"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("encoded manifest missing field %s:\n%s", want, s)
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("not json")); err == nil {
		t.Fatal("Decode(garbage): want error")
	}
}

func TestEntry(t *testing.T) {
	m := sample()
	e, ok := m.Entry("croft-home")
	if !ok || e.DriveFileID != "drv-2" {
		t.Fatalf("Entry(croft-home): got %+v ok=%v", e, ok)
	}
	if _, ok := m.Entry("nope"); ok {
		t.Fatal("Entry(nope): want !ok")
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("hello porter")
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	if err := VerifySHA256(data, want); err != nil {
		t.Fatalf("VerifySHA256(match): %v", err)
	}
	if err := VerifySHA256(data, strings.Repeat("00", 32)); err == nil {
		t.Fatal("VerifySHA256(mismatch): want error")
	}
	if err := VerifySHA256(data, strings.ToUpper(want)); err != nil {
		t.Fatalf("VerifySHA256 should be case-insensitive on hex: %v", err)
	}
}

func TestTimestampFormat(t *testing.T) {
	at := time.Date(2026, 6, 12, 9, 5, 3, 0, time.FixedZone("AEST", 10*3600))
	got := FormatTimestamp(at)
	// Must render in UTC: 09:05:03+10:00 == 23:05:03Z previous day.
	if got != "20260611T230503Z" {
		t.Fatalf("FormatTimestamp: got %q", got)
	}
	back, err := ParseTimestamp(got)
	if err != nil {
		t.Fatalf("ParseTimestamp: %v", err)
	}
	if !back.Equal(at) {
		t.Fatalf("round-trip: got %v want %v", back, at)
	}
	if _, err := ParseTimestamp("2026-06-12"); err == nil {
		t.Fatal("ParseTimestamp(bad): want error")
	}
}

func TestObjectPaths(t *testing.T) {
	if got := SnapshotObjectPath("almanac", "20260612T120000Z"); got != "backups/almanac/20260612T120000Z.casket" {
		t.Fatalf("SnapshotObjectPath: %q", got)
	}
	if got := ManifestObjectPath("20260612T120000Z"); got != "backups/manifests/20260612T120000Z.json.casket" {
		t.Fatalf("ManifestObjectPath: %q", got)
	}
}
