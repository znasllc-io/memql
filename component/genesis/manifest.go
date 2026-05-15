package genesis

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

//go:embed manifest.yaml
var embeddedManifest []byte

// ManifestEntry mirrors one row in memql's secrets manifest. Callers
// only need Name at validation time; the rest is preserved so
// features that want to inspect entry metadata (decoded `genesis
// show`, scope-aware seeding helpers, etc.) can read it without
// re-parsing.
type ManifestEntry struct {
	Name        string `yaml:"name"`
	Scope       string `yaml:"scope"`
	Kind        string `yaml:"kind,omitempty"`
	Default     string `yaml:"default,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// Manifest is the on-disk shape of memql's manifest.yaml. Source is
// not in the yaml -- it's set by LoadManifest to identify which
// fallback layer was used, for diagnostic output.
type Manifest struct {
	Secrets   []ManifestEntry `yaml:"secrets"`
	Variables []ManifestEntry `yaml:"variables"`
	Source    string          `yaml:"-"`
}

// Names returns the required entry names (secrets + variables) in
// manifest order. Strict-superset validation walks this list to
// verify the operator's .env covers the manifest floor.
func (m *Manifest) Names() []string {
	out := make([]string, 0, len(m.Secrets)+len(m.Variables))
	for _, e := range m.Secrets {
		out = append(out, e.Name)
	}
	for _, e := range m.Variables {
		out = append(out, e.Name)
	}
	return out
}

// LoadManifest resolves the manifest in priority order:
//
//  1. flagPath (the --manifest CLI flag or equivalent caller-supplied path)
//  2. MEMQL_MANIFEST_PATH env var
//  3. $MEMQL_REPO/scripts/secrets/manifest.yaml when MEMQL_REPO is set
//     AND the file exists at that path
//  4. The snapshot embedded in this binary
//
// Source is populated with a human-readable label of the path that
// was used, so the calling code can include it in success / error
// output.
func LoadManifest(flagPath string) (*Manifest, error) {
	if flagPath != "" {
		return loadManifestFromFile(flagPath, "--manifest "+flagPath)
	}
	if p := os.Getenv("MEMQL_MANIFEST_PATH"); p != "" {
		return loadManifestFromFile(p, "MEMQL_MANIFEST_PATH="+p)
	}
	if repo := os.Getenv("MEMQL_REPO"); repo != "" {
		path := filepath.Join(repo, "scripts", "secrets", "manifest.yaml")
		if _, err := os.Stat(path); err == nil {
			return loadManifestFromFile(path, "$MEMQL_REPO/scripts/secrets/manifest.yaml")
		}
	}
	return LoadManifestFromBytes(embeddedManifest, "embedded snapshot")
}

// LoadManifestFromBytes parses a manifest from raw YAML. Used by
// callers that have their own manifest source (testing, snapshots,
// alternate distributions).
func LoadManifestFromBytes(data []byte, label string) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse %s: %w", label, err)
	}
	m.Source = label
	return &m, nil
}

func loadManifestFromFile(path, label string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	return LoadManifestFromBytes(data, label)
}

// FindMissing returns the required names not present in entries,
// in input order. An empty result means the .env covers every
// required name; extras above the floor are not flagged here.
func FindMissing(entries []EnvEntry, required []string) []string {
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Name] = true
	}
	var missing []string
	for _, name := range required {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	return missing
}
