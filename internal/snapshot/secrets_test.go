package snapshot

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func fakeSecrets() *fake.Clientset {
	return fake.NewClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "almanac-org-seed", Namespace: "cwb"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"seed": []byte("super-secret-seed")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "nexus-broker-env", Namespace: "nexus"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"ENV": []byte("VALUE=1\n")},
		},
		// A secret NOT in the curated list — must not appear in output.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "cwb"},
			Data:       map[string][]byte{"x": []byte("y")},
		},
	)
}

func TestSnapshotSecrets(t *testing.T) {
	refs := []SecretRef{
		{Namespace: "cwb", Name: "almanac-org-seed"},
		{Namespace: "nexus", Name: "nexus-broker-env"},
	}
	out, err := snapshotSecrets(context.Background(), fakeSecrets(), refs)
	if err != nil {
		t.Fatalf("snapshotSecrets: %v", err)
	}

	// Output is a single multi-doc YAML stream, one doc per curated secret,
	// kubectl-applyable (apiVersion/kind/metadata/type/data).
	docs := strings.Split(string(out), "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2:\n%s", len(docs), out)
	}
	type secretDoc struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]string `json:"metadata"`
		Type       string            `json:"type"`
		Data       map[string]string `json:"data"`
	}
	var first secretDoc
	if err := yaml.Unmarshal([]byte(docs[0]), &first); err != nil {
		t.Fatalf("doc[0] not YAML: %v\n%s", err, docs[0])
	}
	if first.APIVersion != "v1" || first.Kind != "Secret" {
		t.Fatalf("doc[0] header: %+v", first)
	}
	if first.Metadata["name"] != "almanac-org-seed" || first.Metadata["namespace"] != "cwb" {
		t.Fatalf("doc[0] metadata: %+v", first.Metadata)
	}
	if first.Type != string(corev1.SecretTypeOpaque) {
		t.Fatalf("doc[0] type: %q", first.Type)
	}
	// data values are base64 per k8s convention.
	if first.Data["seed"] != "c3VwZXItc2VjcmV0LXNlZWQ=" {
		t.Fatalf("doc[0] data: %+v", first.Data)
	}
	if strings.Contains(string(out), "unrelated") {
		t.Fatal("non-curated secret leaked into snapshot")
	}
	// No noisy server-side metadata.
	for _, banned := range []string{"creationTimestamp", "resourceVersion", "managedFields", "uid:"} {
		if strings.Contains(string(out), banned) {
			t.Errorf("output contains %s:\n%s", banned, out)
		}
	}
}

func TestSnapshotSecretsMissing(t *testing.T) {
	refs := []SecretRef{{Namespace: "cwb", Name: "does-not-exist"}}
	if _, err := snapshotSecrets(context.Background(), fakeSecrets(), refs); err == nil {
		t.Fatal("want error for missing curated secret (partial secret backups are worse than loud failure)")
	}
}
