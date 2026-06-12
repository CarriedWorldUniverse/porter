package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/envelope"
	"github.com/CarriedWorldUniverse/porter/internal/manifest"
	"github.com/CarriedWorldUniverse/porter/internal/snapshot"
)

// runRestore restores a run: fetch + unseal the manifest, then download,
// unseal, hash-verify, and write each (or one chosen) source artifact into
// outDir. It needs ONE recipient private key — the operator recovery key
// alone is enough (the bare-metal drill). Restore never writes into live
// service paths: everything lands under outDir.
func runRestore(ctx context.Context, d *drive.Client, folder, ts, sourceFilter string, privKey []byte, outDir string, log *slog.Logger) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	m, err := fetchManifest(ctx, d, folder, ts, privKey)
	if err != nil {
		return err
	}

	entries := m.Sources
	if sourceFilter != "" {
		e, ok := m.Entry(sourceFilter)
		if !ok {
			return fmt.Errorf("manifest %s has no source %q", ts, sourceFilter)
		}
		entries = []manifest.SourceEntry{e}
	}

	for _, e := range entries {
		// Manifest fields are untrusted input: sealing needs only the
		// recipient PUBLIC keys, so a compromised Drive account could plant
		// a validly sealed manifest. Names and artifact file names must
		// pass the same rules the sync side writes under — a single path
		// segment, nothing that can step outside outDir.
		if err := snapshot.ValidateName(e.Name); err != nil {
			return fmt.Errorf("manifest entry rejected: %w", err)
		}
		if e.Artifact == "" || e.Artifact == "." || e.Artifact == ".." ||
			e.Artifact != filepath.Base(e.Artifact) {
			return fmt.Errorf("source %q: manifest artifact %q rejected: must be a bare file name", e.Name, e.Artifact)
		}
		rc, err := d.Download(ctx, e.DriveFileID)
		if err != nil {
			return fmt.Errorf("source %q: %w", e.Name, err)
		}
		sealed, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("source %q: reading blob: %w", e.Name, err)
		}
		plaintext, err := envelope.Unseal(privKey, sealed, manifest.SnapshotObjectPath(e.Name, ts))
		if err != nil {
			return fmt.Errorf("source %q: unsealing: %w", e.Name, err)
		}
		if err := manifest.VerifySHA256(plaintext, e.SHA256); err != nil {
			return fmt.Errorf("source %q: %w", e.Name, err)
		}
		if int64(len(plaintext)) != e.Size {
			return fmt.Errorf("source %q: size mismatch: got %d, manifest says %d", e.Name, len(plaintext), e.Size)
		}
		out := filepath.Join(outDir, e.Artifact)
		if err := os.WriteFile(out, plaintext, 0o600); err != nil {
			return fmt.Errorf("source %q: writing %s: %w", e.Name, out, err)
		}
		log.Info("source restored",
			"source", e.Name,
			"artifact", out,
			"size", e.Size,
			"sha256_verified", true,
		)
	}
	return nil
}

// fetchManifest locates, downloads, and unseals one run's manifest.
func fetchManifest(ctx context.Context, d *drive.Client, folder, ts string, privKey []byte) (*manifest.Manifest, error) {
	manifestsFolder, err := d.EnsureFolder(ctx, folder+"/manifests")
	if err != nil {
		return nil, fmt.Errorf("locating manifests folder: %w", err)
	}
	files, err := d.List(ctx, manifestsFolder)
	if err != nil {
		return nil, fmt.Errorf("listing manifests: %w", err)
	}
	wantName := manifestDriveName(ts)
	var id string
	var available []string
	for _, f := range files {
		if f.Name == wantName {
			id = f.ID
			break
		}
		available = append(available, f.Name)
	}
	if id == "" {
		return nil, fmt.Errorf("no manifest %s on Drive (have: %v)", wantName, available)
	}
	rc, err := d.Download(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("downloading manifest: %w", err)
	}
	sealed, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("reading manifest blob: %w", err)
	}
	plaintext, err := envelope.Unseal(privKey, sealed, manifest.ManifestObjectPath(ts))
	if err != nil {
		return nil, fmt.Errorf("unsealing manifest: %w", err)
	}
	return manifest.Decode(plaintext)
}

// uploadBytes uploads an in-memory blob (sealed envelopes) to Drive.
func uploadBytes(ctx context.Context, d *drive.Client, name, parentID string, data []byte) (string, error) {
	return d.Upload(ctx, name, parentID, bytes.NewReader(data))
}

// fileBase is filepath.Base, named for intent at call sites.
func fileBase(p string) string { return filepath.Base(p) }
