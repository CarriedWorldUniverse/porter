package main

// End-to-end test of the whole backup chain against the real sync and
// restore code paths: snapshot (live sqlite fixture + directory tree + fake
// k8s secrets) → multi-recipient casket seal → fake-Drive resumable upload →
// sealed manifest → retention prune → restore using ONLY the second
// (recovery) recipient key → hash + content verification. Only the Drive
// HTTP API and the k8s API are faked; everything else is production code.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	_ "modernc.org/sqlite"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/drive/drivetest"
	"github.com/CarriedWorldUniverse/porter/internal/snapshot"
)

const e2eFolder = "CarriedWorld-Porter/backups"

func e2eFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "almanac.db")
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE params (path TEXT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO params VALUES ('cwb/porter/backup/sources', 'the-yaml'), ('cwb/org/seed', 'seed-value')"); err != nil {
		t.Fatal(err)
	}
	return path
}

func e2eFixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "memory"), 0o755))
	must(os.WriteFile(filepath.Join(root, "memory", "MEMORY.md"), []byte("croft memory"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "src", "clone"), 0o755))
	must(os.WriteFile(filepath.Join(root, "src", "clone", "huge.bin"), bytes.Repeat([]byte{7}, 4096), 0o644))
	return root
}

func e2eEnv(t *testing.T, fakeDrive *drivetest.Server, now time.Time) syncEnv {
	t.Helper()
	dc, err := drive.New(context.Background(), nil,
		option.WithEndpoint(fakeDrive.URL()), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	dc.ChunkSize = 256 * 1024

	kube := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "almanac-org-seed", Namespace: "cwb"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"seed": []byte("org-seed-bytes")},
	})

	return syncEnv{
		Drive:  dc,
		Runner: snapshot.Runner{Kube: kube},
		Sources: []snapshot.Source{
			{Name: "almanac", Type: snapshot.TypeSQLite, Path: e2eFixtureDB(t)},
			{Name: "croft-home", Type: snapshot.TypeTar, Path: e2eFixtureTree(t), Excludes: []string{"src"}},
			{Name: "cwb-secrets", Type: snapshot.TypeSecrets, Secrets: []snapshot.SecretRef{{Namespace: "cwb", Name: "almanac-org-seed"}}},
		},
		Folder: e2eFolder,
		Now:    func() time.Time { return now },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestEndToEndBackupAndRecoveryKeyRestore(t *testing.T) {
	fakeDrive := drivetest.New()
	defer fakeDrive.Close()

	clusterPriv, clusterPub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = clusterPriv
	recoveryPriv, recoveryPub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	env := e2eEnv(t, fakeDrive, now)
	env.Recipients = [][]byte{clusterPub, recoveryPub}

	// ---- Arrange retention fixtures: pre-existing old snapshots ----------
	ctx := context.Background()
	almanacFolder, err := env.Drive.EnsureFolder(ctx, e2eFolder+"/almanac")
	if err != nil {
		t.Fatal(err)
	}
	// now = Fri 2026-06-12. Bands: <7d keep; 7–30d keep one weekly keeper per
	// ISO week; >30d delete (even a lone-week keeper).
	recentID := fakeDrive.AddFile(almanacFolder, "20260610T120000Z.casket", []byte("recent"))       // 2d old → within 7d → KEPT
	weekKeeperID := fakeDrive.AddFile(almanacFolder, "20260602T120000Z.casket", []byte("wk"))        // Tue, wk Jun1–7, earliest → KEPT
	weekPrunedID := fakeDrive.AddFile(almanacFolder, "20260604T120000Z.casket", []byte("wk2"))       // Thu, same wk, not earliest → PRUNED
	pastHorizonID := fakeDrive.AddFile(almanacFolder, "20260301T120000Z.casket", []byte("ancient"))  // >30d, lone-week keeper → PRUNED
	strangerID := fakeDrive.AddFile(almanacFolder, "README.txt", []byte("not porter's"))             // unparseable → NEVER touched
	manifestsFolder, err := env.Drive.EnsureFolder(ctx, e2eFolder+"/manifests")
	if err != nil {
		t.Fatal(err)
	}
	oldManifestID := fakeDrive.AddFile(manifestsFolder, "20260301T120000Z.json.casket", []byte("old manifest")) // manifests NEVER pruned

	// ---- Act: one full sync pass -----------------------------------------
	m, err := runSyncPass(ctx, env)
	if err != nil {
		t.Fatalf("runSyncPass: %v", err)
	}

	// ---- Assert: manifest shape ------------------------------------------
	if m.Timestamp != "20260612T120000Z" {
		t.Fatalf("manifest timestamp: %s", m.Timestamp)
	}
	if len(m.Sources) != 3 {
		t.Fatalf("manifest sources: %d", len(m.Sources))
	}
	for _, e := range m.Sources {
		if e.SHA256 == "" || e.Size == 0 || e.DriveFileID == "" || len(e.CasketKeyIDs) != 2 {
			t.Fatalf("manifest entry incomplete: %+v", e)
		}
	}

	// ---- Assert: Drive holds ONLY ciphertext ------------------------------
	for _, name := range []string{"almanac", "croft-home", "cwb-secrets"} {
		entry, ok := m.Entry(name)
		if !ok {
			t.Fatalf("manifest missing %s", name)
		}
		var blob []byte
		for _, f := range fakeDrive.Files() {
			if f.ID == entry.DriveFileID {
				blob = f.Content
			}
		}
		if blob == nil {
			t.Fatalf("%s: blob %s not on drive", name, entry.DriveFileID)
		}
		for _, marker := range []string{"croft memory", "org-seed-bytes", "seed-value", "almanac-org-seed"} {
			if bytes.Contains(blob, []byte(marker)) {
				t.Fatalf("%s: plaintext marker %q visible in Drive blob", name, marker)
			}
		}
	}

	// ---- Assert: retention -------------------------------------------------
	stillThere := map[string]bool{}
	for _, f := range fakeDrive.Files() {
		stillThere[f.ID] = true
	}
	if !stillThere[recentID] {
		t.Fatal("within-7d snapshot was pruned")
	}
	if !stillThere[weekKeeperID] {
		t.Fatal("weekly keeper (earliest of its ISO week) was pruned")
	}
	if stillThere[weekPrunedID] {
		t.Fatal("non-keeper sibling in the same ISO week survived")
	}
	if stillThere[pastHorizonID] {
		t.Fatal("past-horizon snapshot survived (must delete even lone-week keepers)")
	}
	if !stillThere[strangerID] {
		t.Fatal("retention deleted a file it does not own")
	}
	if !stillThere[oldManifestID] {
		t.Fatal("a manifest was pruned — manifests are never pruned")
	}

	// ---- Act: restore with ONLY the recovery key ---------------------------
	outDir := filepath.Join(t.TempDir(), "restore")
	err = runRestore(ctx, env.Drive, e2eFolder, m.Timestamp, "", recoveryPriv, outDir,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("runRestore(recovery key only): %v", err)
	}

	// ---- Assert: restored sqlite db is intact ------------------------------
	dbPath := filepath.Join(outDir, "almanac.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var integ string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integ); err != nil || integ != "ok" {
		t.Fatalf("restored db integrity: %q err=%v", integ, err)
	}
	var v string
	if err := db.QueryRow("SELECT value FROM params WHERE path = 'cwb/org/seed'").Scan(&v); err != nil || v != "seed-value" {
		t.Fatalf("restored db content: %q err=%v", v, err)
	}

	// ---- Assert: restored tar has the memory file, excludes stayed out ----
	tarPath := filepath.Join(outDir, "croft-home.tar.gz")
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var sawMemory bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(hdr.Name, "src") {
			t.Fatalf("excluded path %s in restored tar", hdr.Name)
		}
		if hdr.Name == "memory/MEMORY.md" {
			b, _ := io.ReadAll(tr)
			if string(b) != "croft memory" {
				t.Fatalf("memory content: %q", b)
			}
			sawMemory = true
		}
	}
	if !sawMemory {
		t.Fatal("memory/MEMORY.md missing from restored tar")
	}

	// ---- Assert: restored secrets YAML carries the curated secret ---------
	secrets, err := os.ReadFile(filepath.Join(outDir, "cwb-secrets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secrets), "almanac-org-seed") ||
		!strings.Contains(string(secrets), "b3JnLXNlZWQtYnl0ZXM=") { // base64("org-seed-bytes")
		t.Fatalf("restored secrets doc wrong:\n%s", secrets)
	}

	// ---- Assert: single-source restore -------------------------------------
	oneDir := filepath.Join(t.TempDir(), "one")
	if err := runRestore(ctx, env.Drive, e2eFolder, m.Timestamp, "almanac", recoveryPriv, oneDir,
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("runRestore(--source almanac): %v", err)
	}
	if _, err := os.Stat(filepath.Join(oneDir, "almanac.db")); err != nil {
		t.Fatal("single-source restore missing almanac.db")
	}
	if _, err := os.Stat(filepath.Join(oneDir, "croft-home.tar.gz")); err == nil {
		t.Fatal("single-source restore restored more than asked")
	}

	// ---- Assert: tamper fails loudly ---------------------------------------
	entry, _ := m.Entry("almanac")
	if !fakeDrive.Corrupt(entry.DriveFileID) {
		t.Fatal("could not corrupt blob")
	}
	err = runRestore(ctx, env.Drive, e2eFolder, m.Timestamp, "almanac", recoveryPriv,
		filepath.Join(t.TempDir(), "tampered"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("restore of bit-flipped blob succeeded — AEAD must fail loudly")
	}
	if !strings.Contains(err.Error(), "unseal") && !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("tamper error should come from envelope open, got: %v", err)
	}

	// ---- Assert: a second pass on a later day produces a distinct run -----
	env.Now = func() time.Time { return now.Add(24 * time.Hour) }
	m2, err := runSyncPass(ctx, env)
	if err != nil {
		t.Fatalf("second runSyncPass: %v", err)
	}
	if m2.Timestamp == m.Timestamp {
		t.Fatal("second run reused the first run's timestamp")
	}
	if _, ok := fakeDrive.FileByName(manifestsFolder, manifestDriveName(m2.Timestamp)); !ok {
		t.Fatal("second run's manifest missing")
	}
}

func TestEndToEndWrongKeyCannotRestore(t *testing.T) {
	fakeDrive := drivetest.New()
	defer fakeDrive.Close()
	_, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	strangerPriv, _, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	env := e2eEnv(t, fakeDrive, now)
	env.Recipients = [][]byte{pub}
	ctx := context.Background()
	m, err := runSyncPass(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	err = runRestore(ctx, env.Drive, e2eFolder, m.Timestamp, "", strangerPriv,
		filepath.Join(t.TempDir(), "out"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("restore with a non-recipient key succeeded")
	}
}
