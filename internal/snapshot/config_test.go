package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleConfig = `
- name: almanac
  type: sqlite
  path: /data/almanac/almanac.db
- name: croft-home
  type: tar
  path: /home/croft
  excludes:
    - src
    - work
    - "*.sock"
- name: cwb-secrets
  type: secrets
  secrets:
    - ns: cwb
      name: almanac-org-seed
    - ns: nexus
      name: nexus-broker-env
`

func TestParseConfig(t *testing.T) {
	srcs, err := ParseConfig([]byte(sampleConfig))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(srcs) != 3 {
		t.Fatalf("got %d sources, want 3", len(srcs))
	}
	if srcs[0].Name != "almanac" || srcs[0].Type != TypeSQLite || srcs[0].Path != "/data/almanac/almanac.db" {
		t.Fatalf("source[0]: %+v", srcs[0])
	}
	if srcs[1].Type != TypeTar || len(srcs[1].Excludes) != 3 {
		t.Fatalf("source[1]: %+v", srcs[1])
	}
	if srcs[2].Type != TypeSecrets || len(srcs[2].Secrets) != 2 ||
		srcs[2].Secrets[0].Namespace != "cwb" || srcs[2].Secrets[1].Name != "nexus-broker-env" {
		t.Fatalf("source[2]: %+v", srcs[2])
	}
}

func TestParseConfigValidation(t *testing.T) {
	cases := map[string]string{
		"missing name":         `[{type: sqlite, path: /x}]`,
		"missing type":         `[{name: a, path: /x}]`,
		"unknown type":         `[{name: a, type: zip, path: /x}]`,
		"sqlite without path":  `[{name: a, type: sqlite}]`,
		"tar without path":     `[{name: a, type: tar}]`,
		"secrets without list": `[{name: a, type: secrets}]`,
		"secret missing ns":    `[{name: a, type: secrets, secrets: [{name: s}]}]`,
		"secret missing name":  `[{name: a, type: secrets, secrets: [{ns: cwb}]}]`,
		"duplicate names":      `[{name: a, type: sqlite, path: /x}, {name: a, type: tar, path: /y}]`,
		"name with slash":      `[{name: "a/b", type: sqlite, path: /x}]`,
		"not yaml":             `:[`,
	}
	for label, cfg := range cases {
		if _, err := ParseConfig([]byte(cfg)); err == nil {
			t.Errorf("%s: want error, got nil", label)
		}
	}
}

func TestLoadConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sources.yaml")
	if err := os.WriteFile(p, []byte(sampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	srcs, err := LoadConfigFile(p)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if len(srcs) != 3 {
		t.Fatalf("got %d sources, want 3", len(srcs))
	}
	if _, err := LoadConfigFile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("LoadConfigFile(missing): want error")
	}
}
