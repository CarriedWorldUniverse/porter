// Package snapshot produces per-source consistent snapshot artifacts in a
// working directory: sqlite databases via WAL-safe `VACUUM INTO` (no service
// pause), directory trees via deterministic tar.gz with exclude globs, and a
// curated list of k8s Secrets via client-go as one YAML doc. The source list
// comes from a YAML config (an almanac parameter's value or a local file).
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
)

// Artifact describes one produced snapshot file.
type Artifact struct {
	// Name is the source name.
	Name string
	// Path is the artifact file in the working directory.
	Path string
	// SHA256 is the lowercase hex digest of the artifact (plaintext).
	SHA256 string
	// Size is the artifact size in bytes.
	Size int64
}

// Runner executes snapshots. Kube is only required when a secrets-type
// source is present.
type Runner struct {
	Kube kubernetes.Interface
}

// DefaultMaxTarBytes caps a tar source's staged (compressed) artifact when the
// source sets no max_bytes. Sits well above legitimate sources yet below the
// porter-backup pod's 8Gi work-volume limit, so a runaway tree fails loudly
// (see cappedWriter) instead of OOM-evicting the pod.
const DefaultMaxTarBytes = 4 << 30 // 4 GiB

// maxTarBytes is the source's max_bytes override, or the package default.
func maxTarBytes(src Source) int64 {
	if src.MaxBytes > 0 {
		return src.MaxBytes
	}
	return DefaultMaxTarBytes
}

// Run snapshots one source into workDir and returns the artifact with its
// plaintext hash and size. Artifact file names are <name> + a type-fixed
// extension (.db / .tar.gz / .yaml).
func (r Runner) Run(ctx context.Context, src Source, workDir string) (Artifact, error) {
	var out string
	switch src.Type {
	case TypeSQLite:
		out = filepath.Join(workDir, src.Name+".db")
		if err := snapshotSQLite(ctx, src.Path, out); err != nil {
			return Artifact{}, fmt.Errorf("source %q: %w", src.Name, err)
		}
	case TypeTar:
		out = filepath.Join(workDir, src.Name+".tar.gz")
		if err := snapshotTar(src.Path, out, src.Excludes, src.Includes, maxTarBytes(src)); err != nil {
			return Artifact{}, fmt.Errorf("source %q: %w", src.Name, err)
		}
	case TypeSecrets:
		if r.Kube == nil {
			return Artifact{}, fmt.Errorf("source %q: secrets source configured but no kubernetes client available", src.Name)
		}
		data, err := snapshotSecrets(ctx, r.Kube, src.Secrets)
		if err != nil {
			return Artifact{}, fmt.Errorf("source %q: %w", src.Name, err)
		}
		out = filepath.Join(workDir, src.Name+".yaml")
		if err := os.WriteFile(out, data, 0o600); err != nil {
			return Artifact{}, fmt.Errorf("source %q: writing artifact: %w", src.Name, err)
		}
	default:
		return Artifact{}, fmt.Errorf("source %q: unknown type %q", src.Name, src.Type)
	}

	sum, size, err := hashFile(out)
	if err != nil {
		return Artifact{}, fmt.Errorf("source %q: %w", src.Name, err)
	}
	return Artifact{Name: src.Name, Path: out, SHA256: sum, Size: size}, nil
}

// hashFile returns the hex SHA-256 and size of a file, streaming (artifacts
// can be GB-scale tars).
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
