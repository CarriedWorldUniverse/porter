// Command packstore-smoke is a one-shot live smoke test of the packstore
// Drive backend against real Google Drive. It reads the oauth bundle from
// PORTER_DRIVE_OAUTH_FILE (same format and permission posture as
// porter-backup's bare-metal path), creates/reuses a dedicated smoke folder,
// writes a fresh throwaway store into it, then restores everything through a
// fresh backend with ONLY the second recipient's key and verifies hashes.
// It prints no secret material.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/drivebackend"
)

const (
	smokeFolder = "CarriedWorld-Porter/packstore-smoke"
	packSize    = 2 << 20   // 2 MiB
	chunkSize   = 512 << 10 // 512 KiB
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("packstore-smoke: FAIL: %v", err)
	}
}

func run() error {
	ctx := context.Background()

	oauth, err := loadOAuth()
	if err != nil {
		return err
	}
	client, err := drive.New(ctx, oauth.TokenSource(ctx))
	if err != nil {
		return fmt.Errorf("drive client: %w", err)
	}
	folderID, err := client.EnsureFolder(ctx, smokeFolder)
	if err != nil {
		return fmt.Errorf("ensure folder: %w", err)
	}
	fmt.Printf("folder %s id=%s\n", smokeFolder, folderID)

	if existing, err := client.List(ctx, folderID); err != nil {
		return fmt.Errorf("pre-list: %w", err)
	} else if len(existing) > 0 {
		return fmt.Errorf("smoke folder is not empty (%d objects) — clean it up first, refusing to mix stores", len(existing))
	}

	_, pubA, err := casket.GenerateRecipientKey()
	if err != nil {
		return err
	}
	privB, pubB, err := casket.GenerateRecipientKey()
	if err != nil {
		return err
	}

	// Write path: recipients A+B.
	w, err := packstore.Init(drivebackend.New(ctx, client, folderID), [][]byte{pubA, pubB}, packSize, chunkSize)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	big := make([]byte, 3300000)
	if _, err := rand.Read(big); err != nil {
		return err
	}
	small := make([]byte, 100)
	if _, err := rand.Read(small); err != nil {
		return err
	}
	w.Put("big.bin", big)
	if err := w.Commit(); err != nil {
		return fmt.Errorf("commit 1: %w", err)
	}
	w.Put("small.bin", small)
	if err := w.Commit(); err != nil {
		return fmt.Errorf("commit 2: %w", err)
	}
	fmt.Println("wrote big.bin (3300000 B) + small.bin (100 B) across 2 commits")

	// Recovery path: FRESH backend (empty cache), reader with ONLY key B.
	r, err := packstore.OpenReader(drivebackend.New(ctx, client, folderID), privB)
	if err != nil {
		return fmt.Errorf("open reader (recovery key only): %w", err)
	}
	for name, want := range map[string][]byte{"big.bin": big, "small.bin": small} {
		got, err := r.Get(name)
		if err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
		w, g := sha256.Sum256(want), sha256.Sum256(got)
		if w != g {
			return fmt.Errorf("restore %s: hash mismatch", name)
		}
		fmt.Printf("restored %s recovery-key-only: sha256 %s MATCH\n", name, hex.EncodeToString(g[:8]))
	}

	// Provider view: uniformity straight from Drive's metadata.
	files, err := client.List(ctx, folderID)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	packSizes := map[int64]int{}
	for _, f := range files {
		fmt.Printf("provider sees: %10d B  %s\n", f.Size, f.Name)
		if len(f.Name) > 5 && f.Name[:5] == "pack-" {
			packSizes[f.Size]++
		}
	}
	if len(packSizes) != 1 {
		return fmt.Errorf("pack sizes not uniform: %v", packSizes)
	}
	fmt.Println("PASS: live Drive round-trip, recovery-key-only restore, uniform packs")
	return nil
}

// loadOAuth mirrors porter-backup's bare-metal bundle-file path, including
// the permission check.
func loadOAuth() (drive.OAuth, error) {
	path := os.Getenv("PORTER_DRIVE_OAUTH_FILE")
	if path == "" {
		return drive.OAuth{}, fmt.Errorf("PORTER_DRIVE_OAUTH_FILE not set")
	}
	return drive.OAuthFromBundleFile(path)
}
