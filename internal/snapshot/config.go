package snapshot

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Source types.
const (
	TypeSQLite  = "sqlite"
	TypeTar     = "tar"
	TypeSecrets = "secrets"
)

// SecretRef names one curated k8s Secret to back up.
type SecretRef struct {
	Namespace string `yaml:"ns"`
	Name      string `yaml:"name"`
}

// Source is one backup source from the sources config (the almanac parameter
// `cwb/porter/backup/sources`, or a local YAML file): a top-level YAML list
// of `{name, type, path|secrets, excludes}`.
type Source struct {
	// Name is the source's unique name; it names the artifact and the Drive
	// snapshot folder, so it must be path-segment safe.
	Name string `yaml:"name"`
	// Type is one of sqlite|tar|secrets.
	Type string `yaml:"type"`
	// Path is the sqlite db file (sqlite) or the directory root (tar).
	Path string `yaml:"path,omitempty"`
	// Secrets is the curated namespace/name list (secrets type only).
	Secrets []SecretRef `yaml:"secrets,omitempty"`
	// Excludes are glob patterns pruned from a tar source (see snapshotTar).
	Excludes []string `yaml:"excludes,omitempty"`
}

// ParseConfig parses and validates the sources YAML.
func ParseConfig(data []byte) ([]Source, error) {
	var srcs []Source
	if err := yaml.Unmarshal(data, &srcs); err != nil {
		return nil, fmt.Errorf("parsing sources config: %w", err)
	}
	seen := make(map[string]bool, len(srcs))
	for i, s := range srcs {
		if s.Name == "" {
			return nil, fmt.Errorf("source %d: name is required", i)
		}
		if strings.ContainsAny(s.Name, "/\\ ") {
			return nil, fmt.Errorf("source %q: name must be path-segment safe (no slashes or spaces)", s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("duplicate source name %q", s.Name)
		}
		seen[s.Name] = true
		switch s.Type {
		case TypeSQLite, TypeTar:
			if s.Path == "" {
				return nil, fmt.Errorf("source %q: type %s requires path", s.Name, s.Type)
			}
		case TypeSecrets:
			if len(s.Secrets) == 0 {
				return nil, fmt.Errorf("source %q: type secrets requires a non-empty secrets list", s.Name)
			}
			for j, ref := range s.Secrets {
				if ref.Namespace == "" || ref.Name == "" {
					return nil, fmt.Errorf("source %q: secrets[%d] needs both ns and name", s.Name, j)
				}
			}
		case "":
			return nil, fmt.Errorf("source %q: type is required", s.Name)
		default:
			return nil, fmt.Errorf("source %q: unknown type %q (want %s|%s|%s)", s.Name, s.Type, TypeSQLite, TypeTar, TypeSecrets)
		}
	}
	return srcs, nil
}

// LoadConfigFile reads and parses a sources YAML file.
func LoadConfigFile(path string) ([]Source, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading sources config: %w", err)
	}
	return ParseConfig(data)
}
