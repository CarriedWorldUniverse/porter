package main

// Restore-path hardening tests: a manifest is AEAD-authenticated, but
// SEALING needs only the recipient PUBLIC keys — a compromised Drive account
// plus knowledge of the (non-secret) recipient set can plant a validly
// sealed, malicious manifest. Restore must therefore treat decoded manifest
// fields as untrusted input: artifact names must stay inside the output dir
// and source names must pass the same validation the sources config enforces.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"google.golang.org/api/option"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/drive/drivetest"
	"github.com/CarriedWorldUniverse/porter/internal/envelope"
	"github.com/CarriedWorldUniverse/porter/internal/manifest"
)

// plantManifest seals a crafted manifest + one snapshot blob onto the fake
// Drive exactly as a sync pass would, returning the drive client.
func plantManifest(t *testing.T, fakeDrive *drivetest.Server, m *manifest.Manifest, payload []byte, pub []byte) *drive.Client {
	t.Helper()
	ctx := context.Background()
	dc, err := drive.New(ctx, nil, option.WithEndpoint(fakeDrive.URL()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}

	// Upload the snapshot blob under the FIRST entry's coordinates.
	e := &m.Sources[0]
	srcFolder, err := dc.EnsureFolder(ctx, e2eFolder+"/"+"planted")
	if err != nil {
		t.Fatal(err)
	}
	sealedBlob, err := envelope.Seal(payload, [][]byte{pub}, manifest.SnapshotObjectPath(e.Name, m.Timestamp))
	if err != nil {
		t.Fatal(err)
	}
	e.DriveFileID = fakeDrive.AddFile(srcFolder, snapshotDriveName(m.Timestamp), sealedBlob)

	encoded, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	sealedManifest, err := envelope.Seal(encoded, [][]byte{pub}, manifest.ManifestObjectPath(m.Timestamp))
	if err != nil {
		t.Fatal(err)
	}
	manifestsFolder, err := dc.EnsureFolder(ctx, e2eFolder+"/manifests")
	if err != nil {
		t.Fatal(err)
	}
	fakeDrive.AddFile(manifestsFolder, manifestDriveName(m.Timestamp), sealedManifest)
	return dc
}

func TestRestoreRejectsTraversalArtifact(t *testing.T) {
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("malicious payload")
	sum := sha256Hex(payload)
	ts := manifest.FormatTimestamp(time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC))

	for _, artifact := range []string{
		"../escaped.txt",
		"a/../../escaped.txt",
		"/etc/porter-evil",
		"sub/dir.txt", // even plain subpaths are not what sync writes — reject
		"..",
		"",
	} {
		t.Run(artifact, func(t *testing.T) {
			fakeDrive := drivetest.New()
			defer fakeDrive.Close()
			base := t.TempDir()
			outDir := filepath.Join(base, "restore")
			escapePath := filepath.Join(base, "escaped.txt")

			m := &manifest.Manifest{
				Timestamp: ts,
				Sources: []manifest.SourceEntry{{
					Name: "planted", Artifact: artifact,
					SHA256: sum, Size: int64(len(payload)),
				}},
			}
			dc := plantManifest(t, fakeDrive, m, payload, pub)
			err := runRestore(context.Background(), dc, e2eFolder, ts, "", priv, outDir,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil {
				t.Fatalf("artifact %q: restore accepted a non-basename artifact", artifact)
			}
			if !strings.Contains(err.Error(), "artifact") {
				t.Fatalf("artifact %q: error should name the artifact field: %v", artifact, err)
			}
			if _, statErr := os.Stat(escapePath); statErr == nil {
				t.Fatalf("artifact %q: file written OUTSIDE the output dir", artifact)
			}
		})
	}
}

func TestRestoreRejectsInvalidSourceName(t *testing.T) {
	fakeDrive := drivetest.New()
	defer fakeDrive.Close()
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("x")
	ts := manifest.FormatTimestamp(time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC))
	m := &manifest.Manifest{
		Timestamp: ts,
		Sources: []manifest.SourceEntry{{
			Name: "../sneaky", Artifact: "ok.db",
			SHA256: sha256Hex(payload), Size: 1,
		}},
	}
	dc := plantManifest(t, fakeDrive, m, payload, pub)
	err = runRestore(context.Background(), dc, e2eFolder, ts, "", priv, t.TempDir(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("restore accepted a manifest source name that the sources config would reject")
	}
}

func TestDriveOAuthRejectsLooseFilePerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bundle.json")
	raw := `{"client_id":"cid","client_secret":"cs","refresh_token":"rt","token_uri":"https://t","scope":"s"}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	// Explicit chmod: the process umask may otherwise tighten the mode.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTER_DRIVE_OAUTH_FILE", p)
	_, err := driveOAuth(t.Context())
	if err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("world-readable oauth bundle file accepted: %v", err)
	}
	// 0600 is fine.
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := driveOAuth(t.Context()); err != nil {
		t.Fatalf("0600 oauth bundle rejected: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
