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

// ManifestEntry mirrors one row in memql's secrets/variable registry.
// Callers only need Name at seal-validation time; the rest is the
// authoritative metadata that drives the cockpit Configuration screen
// (Epic 7 / memql#2104), boot-time fail-fast validation (#2108), and
// first-deploy injection (#2109) so all three read one source of truth.
//
// Two requiredness axes are deliberately separate:
//
//   - Optional drives the SEAL FLOOR (`Names()`): the strict-superset
//     set a developer's .env must cover before `genesis-seal` will
//     produce an envelope. It stays a small curated set so registering
//     the full ~240-var universe here does not break local sealing --
//     every newly-registered entry sets `optional: true`.
//   - Required drives BOOT VALIDATION (#2108): the node types that must
//     have this var present at startup. A var can be Optional for
//     sealing (defaulted / node-conditional) yet Required by a specific
//     node type at boot. The two axes never conflict.
type ManifestEntry struct {
	Name string `yaml:"name"`
	// Component is the owning subsystem (engine, identity, email,
	// polyphon, voice, database, ai, avatar, telephony, transport, ...).
	// Groups entries in the Configuration screen and the audit table.
	Component   string `yaml:"component,omitempty"`
	Scope       string `yaml:"scope"`
	Kind        string `yaml:"kind,omitempty"`
	Default     string `yaml:"default,omitempty"`
	Description string `yaml:"description,omitempty"`
	// Required lists the node types that require this var at boot. The
	// sentinel "all" means every node type. An empty list means no node
	// strictly requires it (purely optional / defaulted). Consumed by
	// the fail-fast boot validator (#2108) keyed on MEMQL_NODE_TYPE.
	Required []string `yaml:"required,omitempty"`
	// Optional entries are documented + sealed-when-present, but are NOT
	// part of the strict-superset seal floor: a .env that omits them still
	// seals. Use for secrets/vars that only some deployments carry (e.g.
	// the identity envMode signing seed MEMQL_IDENTITY_SIGNING_KEY_B64, which
	// staging/prod need for HA but local-dev disk-key mode does not), or
	// any var outside the curated dev-seal floor. memql#619 / #2104.
	Optional bool `yaml:"optional,omitempty"`
}

// RequiredFor reports whether this entry is required at boot for the
// given node type. The "all" sentinel matches every node type. Drives
// the fail-fast boot validator (#2108).
func (e ManifestEntry) RequiredFor(nodeType string) bool {
	for _, nt := range e.Required {
		if nt == "all" || nt == nodeType {
			return true
		}
	}
	return false
}

// Manifest is the on-disk shape of memql's manifest.yaml. Source is
// not in the yaml -- it's set by LoadManifest to identify which
// fallback layer was used, for diagnostic output.
type Manifest struct {
	Secrets   []ManifestEntry `yaml:"secrets"`
	Variables []ManifestEntry `yaml:"variables"`
	Source    string          `yaml:"-"`
}

// Names returns the REQUIRED entry names (secrets + variables) in
// manifest order. Strict-superset validation walks this list to
// verify the operator's .env covers the manifest floor. Entries marked
// `optional: true` are excluded -- they are documented + sealed when
// present, but never required (memql#619).
func (m *Manifest) Names() []string {
	out := make([]string, 0, len(m.Secrets)+len(m.Variables))
	for _, e := range m.Secrets {
		if e.Optional {
			continue
		}
		out = append(out, e.Name)
	}
	for _, e := range m.Variables {
		if e.Optional {
			continue
		}
		out = append(out, e.Name)
	}
	return out
}

// AllEntries returns every registry entry (secrets followed by
// variables) in manifest order. Consumed by the cockpit Configuration
// screen (#2110), first-deploy injection (#2109), and the boot
// validator (#2108), which all need the full set rather than just the
// seal floor.
func (m *Manifest) AllEntries() []ManifestEntry {
	out := make([]ManifestEntry, 0, len(m.Secrets)+len(m.Variables))
	out = append(out, m.Secrets...)
	out = append(out, m.Variables...)
	return out
}

// InjectableDefault is one manifest entry selected for first-deploy
// default injection (#2109), tagged with whether it came from the
// `secrets:` or `variables:` list (a distinction AllEntries() flattens
// away). The injector seeds the entry's Default as a cluster
// variable/secret row ONLY IF the value is absent, so an operator edit
// is never overwritten.
type InjectableDefault struct {
	Entry    ManifestEntry
	IsSecret bool
}

// InjectableDefaults returns the manifest entries eligible for
// first-deploy default injection (#2109): entries that carry a
// non-empty Default AND a global/partition scope. Node-scoped entries
// are per-pod process env (k8s/shell) and are NEVER concept-stored, so
// they are skipped here. Secrets are listed before variables, each
// tagged so the injector picks the right write path (global vs
// partition secret/variable). The selection is pure and side-effect
// free; the only-if-absent existence check happens at inject time.
func (m *Manifest) InjectableDefaults() []InjectableDefault {
	var out []InjectableDefault
	for _, e := range m.Secrets {
		if isInjectableEntry(e) {
			out = append(out, InjectableDefault{Entry: e, IsSecret: true})
		}
	}
	for _, e := range m.Variables {
		if isInjectableEntry(e) {
			out = append(out, InjectableDefault{Entry: e, IsSecret: false})
		}
	}
	return out
}

// isInjectableEntry reports whether an entry is a first-deploy
// injection candidate: it must carry a non-empty Default and a scope of
// either "global" or "partition". Node-scoped entries are excluded --
// they live in process env, not concept storage.
func isInjectableEntry(e ManifestEntry) bool {
	if e.Default == "" {
		return false
	}
	switch e.Scope {
	case "global", "partition":
		return true
	default:
		return false
	}
}

// Lookup returns the entry with the given name (searching secrets then
// variables) and whether it was found.
func (m *Manifest) Lookup(name string) (ManifestEntry, bool) {
	for _, e := range m.Secrets {
		if e.Name == name {
			return e, true
		}
	}
	for _, e := range m.Variables {
		if e.Name == name {
			return e, true
		}
	}
	return ManifestEntry{}, false
}

// RequiredForNodeType returns the entry names required at boot for the
// given MEMQL_NODE_TYPE, in manifest order. Powers the fail-fast boot
// validator (#2108).
func (m *Manifest) RequiredForNodeType(nodeType string) []string {
	var out []string
	for _, e := range m.AllEntries() {
		if e.RequiredFor(nodeType) {
			out = append(out, e.Name)
		}
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

// FindMissingWithLegacy is the legacy-tolerant variant of FindMissing
// used by Seal (Epic 7.3 / memql#2106). A floor name counts as present
// if the .env carries either the new name OR its legacy alias, so an
// operator's pre-7.3 .env (still using the old IDENTITY_* / POLYPHON_* /
// ... names) seals without forcing an immediate rename. The boot-time
// shim ApplyLegacyEnvAliases bridges the value at runtime.
func FindMissingWithLegacy(entries []EnvEntry, required []string) []string {
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Name] = true
	}
	var missing []string
	for _, name := range required {
		if !PresentWithLegacy(have, name) {
			missing = append(missing, name)
		}
	}
	return missing
}
