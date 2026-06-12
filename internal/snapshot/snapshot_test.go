package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes"
)

func checkArtifact(t *testing.T, a Artifact, wantFile string) {
	t.Helper()
	if filepath.Base(a.Path) != wantFile {
		t.Fatalf("artifact file: got %s want %s", filepath.Base(a.Path), wantFile)
	}
	b, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatalf("artifact unreadable: %v", err)
	}
	if int64(len(b)) != a.Size {
		t.Fatalf("size: got %d, file is %d", a.Size, len(b))
	}
	sum := sha256.Sum256(b)
	if a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256 mismatch: %s", a.SHA256)
	}
}

func TestRunSQLite(t *testing.T) {
	dbPath, _ := newFixtureDB(t, 3)
	r := Runner{}
	a, err := r.Run(context.Background(), Source{Name: "almanac", Type: TypeSQLite, Path: dbPath}, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.Name != "almanac" {
		t.Fatalf("name: %s", a.Name)
	}
	checkArtifact(t, a, "almanac.db")
}

func TestRunTar(t *testing.T) {
	root := fixtureTree(t)
	r := Runner{}
	a, err := r.Run(context.Background(), Source{Name: "croft-home", Type: TypeTar, Path: root, Excludes: []string{"src"}}, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	checkArtifact(t, a, "croft-home.tar.gz")
}

func TestRunSecrets(t *testing.T) {
	r := Runner{Kube: fakeSecrets()}
	a, err := r.Run(context.Background(), Source{
		Name: "cwb-secrets", Type: TypeSecrets,
		Secrets: []SecretRef{{Namespace: "cwb", Name: "almanac-org-seed"}},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	checkArtifact(t, a, "cwb-secrets.yaml")
}

func TestRunSecretsWithoutKubeClient(t *testing.T) {
	r := Runner{Kube: kubernetes.Interface(nil)}
	_, err := r.Run(context.Background(), Source{
		Name: "s", Type: TypeSecrets,
		Secrets: []SecretRef{{Namespace: "cwb", Name: "x"}},
	}, t.TempDir())
	if err == nil {
		t.Fatal("want error when secrets source configured but no kube client")
	}
}

func TestRunUnknownType(t *testing.T) {
	r := Runner{}
	if _, err := r.Run(context.Background(), Source{Name: "a", Type: "zip", Path: "/x"}, t.TempDir()); err == nil {
		t.Fatal("want error for unknown type")
	}
}
