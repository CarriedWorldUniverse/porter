package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/envelope"
	"github.com/CarriedWorldUniverse/porter/internal/manifest"
	"github.com/CarriedWorldUniverse/porter/internal/retention"
	"github.com/CarriedWorldUniverse/porter/internal/snapshot"
)

// syncEnv is everything one sync pass needs — injected so the end-to-end
// test can wire a fake Drive and fixture sources through the REAL pass.
type syncEnv struct {
	Drive      *drive.Client
	Runner     snapshot.Runner
	Sources    []snapshot.Source
	Recipients [][]byte
	// Folder is the Drive base folder path (PORTER_DRIVE_FOLDER).
	Folder string
	Now    func() time.Time
	Log    *slog.Logger
}

// runSyncPass executes one full backup pass: snapshot each source → seal →
// upload → write the sealed manifest → prune per retention. It returns the
// run's manifest. A source failure aborts the pass (a partial backup set
// with a manifest claiming otherwise is worse than a loud retry next tick).
func runSyncPass(ctx context.Context, env syncEnv) (*manifest.Manifest, error) {
	ts := manifest.FormatTimestamp(env.Now())
	workDir, err := os.MkdirTemp("", "porter-backup-*")
	if err != nil {
		return nil, fmt.Errorf("creating work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	m := &manifest.Manifest{Timestamp: ts}
	for _, src := range env.Sources {
		start := time.Now()
		art, err := env.Runner.Run(ctx, src, workDir)
		if err != nil {
			return nil, fmt.Errorf("snapshotting %q: %w", src.Name, err)
		}
		plaintext, err := os.ReadFile(art.Path)
		if err != nil {
			return nil, fmt.Errorf("reading artifact for %q: %w", src.Name, err)
		}
		objectPath := manifest.SnapshotObjectPath(src.Name, ts)
		sealed, err := envelope.Seal(plaintext, env.Recipients, objectPath)
		if err != nil {
			return nil, fmt.Errorf("sealing %q: %w", src.Name, err)
		}
		keyIDs, err := envelope.RecipientIDsHex(sealed)
		if err != nil {
			return nil, fmt.Errorf("reading recipient ids for %q: %w", src.Name, err)
		}
		folderID, err := env.Drive.EnsureFolder(ctx, env.Folder+"/"+src.Name)
		if err != nil {
			return nil, fmt.Errorf("ensuring Drive folder for %q: %w", src.Name, err)
		}
		fileID, err := uploadBytes(ctx, env.Drive, snapshotDriveName(ts), folderID, sealed)
		if err != nil {
			return nil, fmt.Errorf("uploading %q: %w", src.Name, err)
		}
		m.Sources = append(m.Sources, manifest.SourceEntry{
			Name:         src.Name,
			Artifact:     fileBase(art.Path),
			SHA256:       art.SHA256,
			Size:         art.Size,
			DriveFileID:  fileID,
			CasketKeyIDs: keyIDs,
		})
		env.Log.Info("source backed up",
			"source", src.Name,
			"size", art.Size,
			"sealed_size", len(sealed),
			"duration", time.Since(start).Round(time.Millisecond).String(),
			"drive_file_id", fileID,
		)
	}

	// Seal + upload the manifest itself.
	encoded, err := m.Encode()
	if err != nil {
		return nil, fmt.Errorf("encoding manifest: %w", err)
	}
	sealedManifest, err := envelope.Seal(encoded, env.Recipients, manifest.ManifestObjectPath(ts))
	if err != nil {
		return nil, fmt.Errorf("sealing manifest: %w", err)
	}
	manifestsFolder, err := env.Drive.EnsureFolder(ctx, env.Folder+"/manifests")
	if err != nil {
		return nil, fmt.Errorf("ensuring manifests folder: %w", err)
	}
	manifestID, err := uploadBytes(ctx, env.Drive, manifestDriveName(ts), manifestsFolder, sealedManifest)
	if err != nil {
		return nil, fmt.Errorf("uploading manifest: %w", err)
	}
	env.Log.Info("manifest uploaded", "timestamp", ts, "sources", len(m.Sources), "drive_file_id", manifestID)

	if err := prune(ctx, env); err != nil {
		// The backup itself succeeded — surface the prune failure without
		// failing the pass.
		env.Log.Error("retention prune failed", "error", err.Error())
	}
	return m, nil
}

// prune applies the retention policy to every source's snapshot folder.
// Manifests are never pruned. Files whose names don't parse as snapshot
// timestamps are never touched.
func prune(ctx context.Context, env syncEnv) error {
	now := env.Now()
	for _, src := range env.Sources {
		folderID, err := env.Drive.EnsureFolder(ctx, env.Folder+"/"+src.Name)
		if err != nil {
			return fmt.Errorf("source %q: %w", src.Name, err)
		}
		files, err := env.Drive.List(ctx, folderID)
		if err != nil {
			return fmt.Errorf("source %q: %w", src.Name, err)
		}
		var items []retention.Item
		for _, f := range files {
			if t, ok := parseSnapshotName(f.Name); ok {
				items = append(items, retention.Item{ID: f.ID, Time: t})
			}
		}
		for _, doomed := range retention.ToDelete(items, now) {
			if err := env.Drive.Delete(ctx, doomed.ID); err != nil {
				return fmt.Errorf("source %q: deleting %s: %w", src.Name, doomed.ID, err)
			}
			env.Log.Info("pruned snapshot", "source", src.Name, "drive_file_id", doomed.ID, "age_days", int(now.Sub(doomed.Time).Hours()/24))
		}
	}
	return nil
}
