package snapshot

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// secretDoc is the clean, kubectl-applyable YAML shape we emit per secret —
// the durable fields only (no resourceVersion/uid/managedFields noise that
// would churn the artifact and block a clean re-apply).
type secretDoc struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   secretMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
}

type secretMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// snapshotSecrets reads the CURATED list of k8s Secrets (namespace/name
// pairs — never a blanket namespace dump) and renders them as one multi-doc
// YAML stream, restorable with `kubectl apply -f`. Any missing secret fails
// the whole snapshot: a silently partial secrets backup is worse than a loud
// failure.
func snapshotSecrets(ctx context.Context, client kubernetes.Interface, refs []SecretRef) ([]byte, error) {
	var buf bytes.Buffer
	for i, ref := range refs {
		sec, err := client.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading secret %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		doc := secretDoc{
			APIVersion: "v1",
			Kind:       "Secret",
			Metadata: secretMeta{
				Name:        sec.Name,
				Namespace:   sec.Namespace,
				Labels:      sec.Labels,
				Annotations: sec.Annotations,
			},
			Type: string(sec.Type),
			Data: make(map[string]string, len(sec.Data)),
		}
		for k, v := range sec.Data {
			doc.Data[k] = base64.StdEncoding.EncodeToString(v)
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("rendering secret %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(out)
	}
	return buf.Bytes(), nil
}
