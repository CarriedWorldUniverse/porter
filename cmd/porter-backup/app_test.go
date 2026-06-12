package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/porter/internal/envelope"
)

func TestParseRecipients(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	k2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))

	got, err := parseRecipients(k1 + ", " + k2)
	if err != nil {
		t.Fatalf("parseRecipients: %v", err)
	}
	if len(got) != 2 || got[0][0] != 1 || got[1][0] != 2 {
		t.Fatalf("parsed: %v", got)
	}

	for label, in := range map[string]string{
		"empty":      "",
		"not base64": "!!!",
		"wrong size": base64.StdEncoding.EncodeToString([]byte("short")),
	} {
		if _, err := parseRecipients(in); err == nil {
			t.Errorf("%s: want error", label)
		}
	}
}

func TestParseSnapshotName(t *testing.T) {
	ts, ok := parseSnapshotName("20260612T120000Z.casket")
	if !ok {
		t.Fatal("want ok for valid snapshot name")
	}
	if ts.UTC() != time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC) {
		t.Fatalf("parsed time: %v", ts)
	}
	for _, bad := range []string{
		"20260612T120000Z.json.casket", // a manifest — NEVER prunable
		"random.casket",
		"20260612T120000Z",
		"notes.txt",
	} {
		if _, ok := parseSnapshotName(bad); ok {
			t.Errorf("parseSnapshotName(%q): want !ok", bad)
		}
	}
}

func TestKeygenRoundTrip(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "recovery.key")
	var out bytes.Buffer
	if err := runKeygen(keyPath, &out); err != nil {
		t.Fatalf("runKeygen: %v", err)
	}

	// Private key file: 0600, base64, 32 bytes.
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perms: %o, want 0600", perm)
	}
	priv, err := readPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("readPrivateKey: %v", err)
	}

	// Stdout: the base64 public key, usable as a PORTER_RECIPIENTS entry —
	// prove it by sealing to it and opening with the private key file.
	pubB64 := strings.TrimSpace(out.String())
	recipients, err := parseRecipients(pubB64)
	if err != nil {
		t.Fatalf("printed pubkey not a valid recipient: %v", err)
	}
	blob, err := envelope.Seal([]byte("drill"), recipients, "p")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := envelope.Unseal(priv, blob, "p")
	if err != nil || string(pt) != "drill" {
		t.Fatalf("keygen keypair does not round-trip: %v", err)
	}

	// Refuses to overwrite an existing key file.
	if err := runKeygen(keyPath, &out); err == nil {
		t.Fatal("runKeygen over existing file: want error")
	}
}

func TestReadPrivateKeyErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(bad, []byte("not-base64!!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKey(bad); err == nil {
		t.Fatal("want error for non-base64 key file")
	}
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte(base64.StdEncoding.EncodeToString([]byte("xy"))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKey(short); err == nil {
		t.Fatal("want error for wrong-size key")
	}
	if _, err := readPrivateKey(filepath.Join(dir, "absent.key")); err == nil {
		t.Fatal("want error for missing key file")
	}
}

func TestIntervalParsing(t *testing.T) {
	t.Setenv("PORTER_INTERVAL", "")
	d, err := interval()
	if err != nil || d != 6*time.Hour {
		t.Fatalf("default interval: %v %v", d, err)
	}
	t.Setenv("PORTER_INTERVAL", "90m")
	if d, err = interval(); err != nil || d != 90*time.Minute {
		t.Fatalf("90m: %v %v", d, err)
	}
	t.Setenv("PORTER_INTERVAL", "banana")
	if _, err = interval(); err == nil {
		t.Fatal("want error for bad interval")
	}
	t.Setenv("PORTER_INTERVAL", "-1h")
	if _, err = interval(); err == nil {
		t.Fatal("want error for negative interval")
	}
}

func TestDriveOAuthFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bundle.json")
	raw := `{"client_id":"cid","client_secret":"cs","refresh_token":"rt","token_uri":"https://t","scope":"s"}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTER_DRIVE_OAUTH_FILE", p)
	o, err := driveOAuth(t.Context())
	if err != nil {
		t.Fatalf("driveOAuth: %v", err)
	}
	if o.ClientID != "cid" || o.ClientSecret != "cs" || o.RefreshToken != "rt" || o.TokenURI != "https://t" || o.Scope != "s" {
		t.Fatalf("bundle: %+v", o)
	}
}

func TestDriveOAuthUnconfigured(t *testing.T) {
	t.Setenv("PORTER_DRIVE_OAUTH_FILE", "")
	t.Setenv("CUSTODIAN_GRPC_ADDR", "")
	if _, err := driveOAuth(t.Context()); err == nil {
		t.Fatal("want error with neither custodian nor bundle file configured")
	}
}
