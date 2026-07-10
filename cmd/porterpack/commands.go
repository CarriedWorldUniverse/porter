package main

import (
	"context"
	"fmt"
	"os"

	casket "github.com/CarriedWorldUniverse/casket-go"

	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/gitreplica"
	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/drivebackend"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/localdir"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/s3backend"
	"github.com/CarriedWorldUniverse/porter/internal/s3"
)

// cmdKeygen generates a fresh X25519 recipient keypair and writes the raw
// 32-byte private and public keys to outPrefix.key and outPrefix.pub
// (0600).
func cmdKeygen(outPrefix string) error {
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		return fmt.Errorf("generating recipient key: %w", err)
	}
	if err := writeKeyFile(outPrefix+".key", priv); err != nil {
		return err
	}
	if err := writeKeyFile(outPrefix+".pub", pub); err != nil {
		return err
	}
	fmt.Printf("wrote %s.key (private, 0600) and %s.pub\n", outPrefix, outPrefix)
	return nil
}

func writeKeyFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// readKeyFile reads a raw 32-byte key file (private or public) as written
// by cmdKeygen.
func readKeyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading key file %s: %w", path, err)
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("key file %s: want 32 raw bytes, got %d", path, len(data))
	}
	return data, nil
}

// readRecipients reads a set of public key files (repeatable -recipient).
func readRecipients(paths []string) ([][]byte, error) {
	out := make([][]byte, 0, len(paths))
	for _, p := range paths {
		pub, err := readKeyFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, pub)
	}
	return out, nil
}

func cmdInit(storeDir string, recipientFiles []string, packSize, chunkSize int) error {
	recipients, err := readRecipients(recipientFiles)
	if err != nil {
		return err
	}
	b, err := localdir.New(storeDir)
	if err != nil {
		return err
	}
	if _, err := packstore.Init(b, recipients, packSize, chunkSize); err != nil {
		return fmt.Errorf("initializing store: %w", err)
	}
	fmt.Printf("initialized packstore at %s (pack_size=%d chunk_size=%d)\n", storeDir, packSize, chunkSize)
	return nil
}

func cmdPut(storeDir, keyFile string, recipientFiles []string, name, inFile string) error {
	priv, err := readKeyFile(keyFile)
	if err != nil {
		return err
	}
	recipients, err := readRecipients(recipientFiles)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(inFile)
	if err != nil {
		return fmt.Errorf("reading input file %s: %w", inFile, err)
	}
	b, err := localdir.New(storeDir)
	if err != nil {
		return err
	}
	w, err := packstore.OpenWriter(b, priv, recipients)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	w.Put(name, data)
	if err := w.Commit(); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	fmt.Printf("put %s (%d bytes) as %q\n", inFile, len(data), name)
	return nil
}

func cmdLs(storeDir, keyFile string) error {
	priv, err := readKeyFile(keyFile)
	if err != nil {
		return err
	}
	b, err := localdir.New(storeDir)
	if err != nil {
		return err
	}
	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	for _, name := range r.List() {
		fmt.Println(name)
	}
	return nil
}

func cmdGet(storeDir, keyFile, name, outFile string) error {
	priv, err := readKeyFile(keyFile)
	if err != nil {
		return err
	}
	b, err := localdir.New(storeDir)
	if err != nil {
		return err
	}
	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	data, err := r.Get(name)
	if err != nil {
		return fmt.Errorf("getting %q: %w", name, err)
	}
	if err := os.WriteFile(outFile, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", outFile, err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", outFile, len(data))
	return nil
}

func cmdRepoSnapshot(storeDir, keyFile string, recipientFiles []string, name, repoPath string) error {
	priv, err := readKeyFile(keyFile)
	if err != nil {
		return err
	}
	recipients, err := readRecipients(recipientFiles)
	if err != nil {
		return err
	}
	b, err := localdir.New(storeDir)
	if err != nil {
		return err
	}
	w, err := packstore.OpenWriter(b, priv, recipients)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	seq, refsCount, noChange, err := gitreplica.Snapshot(w, name, repoPath)
	if err != nil {
		return fmt.Errorf("snapshotting %q: %w", name, err)
	}
	if noChange {
		fmt.Printf("replica %q unchanged (seq=%d, %d refs)\n", name, seq, refsCount)
		return nil
	}
	fmt.Printf("snapshotted replica %q as bundle-%08d (%d refs)\n", name, seq, refsCount)
	return nil
}

// cmdMirror copies every object from the localdir store at fromDir onto
// dst, printing one summary line. It carries ciphertext only — no key file
// is needed or accepted.
func cmdMirror(fromDir string, dst packstore.Backend, destDesc string) error {
	src, err := localdir.New(fromDir)
	if err != nil {
		return err
	}
	copied, skipped, err := packstore.Mirror(src, dst)
	if err != nil {
		return fmt.Errorf("mirroring: %w", err)
	}
	fmt.Printf("mirrored %d objects (%d already present) to %s\n", copied, skipped, destDesc)
	return nil
}

// driveBackendFromEnv builds a packstore.Backend backed by Google Drive
// under folderPath, using the oauth bundle named by PORTER_DRIVE_OAUTH_FILE.
func driveBackendFromEnv(ctx context.Context, folderPath string) (packstore.Backend, error) {
	path := os.Getenv("PORTER_DRIVE_OAUTH_FILE")
	if path == "" {
		return nil, fmt.Errorf("PORTER_DRIVE_OAUTH_FILE not set")
	}
	oauth, err := drive.OAuthFromBundleFile(path)
	if err != nil {
		return nil, err
	}
	client, err := drive.New(ctx, oauth.TokenSource(ctx))
	if err != nil {
		return nil, fmt.Errorf("drive client: %w", err)
	}
	folderID, err := client.EnsureFolder(ctx, folderPath)
	if err != nil {
		return nil, fmt.Errorf("ensure folder %q: %w", folderPath, err)
	}
	return drivebackend.New(ctx, client, folderID), nil
}

// s3BackendFromEnv builds a packstore.Backend backed by an S3-compatible
// bucket under keyPrefix, using the credentials file named by
// PORTER_S3_CREDS_FILE. It also returns an "s3://<bucket>/<prefix>"
// description for summary output.
func s3BackendFromEnv(ctx context.Context, keyPrefix string) (packstore.Backend, string, error) {
	path := os.Getenv("PORTER_S3_CREDS_FILE")
	if path == "" {
		return nil, "", fmt.Errorf("PORTER_S3_CREDS_FILE not set")
	}
	creds, err := s3.CredentialsFromFile(path)
	if err != nil {
		return nil, "", err
	}
	client := s3.New(creds, nil)
	dst := s3backend.New(ctx, client, keyPrefix)
	return dst, fmt.Sprintf("s3://%s/%s", creds.Bucket, keyPrefix), nil
}

func cmdRepoRestore(storeDir, keyFile, name, outPath string) error {
	priv, err := readKeyFile(keyFile)
	if err != nil {
		return err
	}
	b, err := localdir.New(storeDir)
	if err != nil {
		return err
	}
	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	if err := gitreplica.Restore(r, name, outPath); err != nil {
		return fmt.Errorf("restoring %q: %w", name, err)
	}
	fmt.Printf("restored replica %q to %s\n", name, outPath)
	return nil
}
