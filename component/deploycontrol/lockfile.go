package deploycontrol

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Lockfile is the typed projection of a release lockfile
// (releases/<version>.yaml) -- the ATOMIC unit of promotion
// (deployment-v2 Phase 4, znasllc-io/memql#702).
//
// The components map is keyed by the short component name (no acr
// prefix); the two cross-repo entries (memql-bff-copresent + copresent)
// additionally carry builtAgainstEngine, which the coherence gate
// requires to equal EngineVersion.
type Lockfile struct {
	// Version is the release version.
	Version string
	// ValidatedAt is the RFC3339 timestamp stamped when the deep gate
	// passed.
	ValidatedAt string
	// Gate is the gate provenance string.
	Gate string
	// EngineVersion is the engine version the carrier + SPA were built
	// against.
	EngineVersion string
	// Components maps short component name -> its pinned digest + repo.
	Components map[string]LockfileComponent
}

// LockfileComponent is one entry of the lockfile's components map.
type LockfileComponent struct {
	// Repo is the source repository (e.g. "znasllc-io/memql").
	Repo string
	// Digest is the pinned "sha256:<64hex>" image authority.
	Digest string
	// BuiltAgainstEngine is set only on the cross-repo components
	// (memql-bff-copresent + copresent); must equal EngineVersion.
	BuiltAgainstEngine string
}

// lockfileDoc mirrors the on-disk lockfile YAML.
type lockfileDoc struct {
	Version       string `yaml:"version"`
	ValidatedAt   string `yaml:"validatedAt"`
	Gate          string `yaml:"gate"`
	EngineVersion string `yaml:"engineVersion"`
	Components    map[string]struct {
		Repo               string `yaml:"repo"`
		Digest             string `yaml:"digest"`
		BuiltAgainstEngine string `yaml:"builtAgainstEngine"`
	} `yaml:"components"`
}

// ParseLockfile parses a release lockfile's raw bytes into a Lockfile.
func ParseLockfile(raw []byte) (Lockfile, error) {
	var doc lockfileDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Lockfile{}, fmt.Errorf("deploycontrol: parse lockfile yaml: %w", err)
	}

	lf := Lockfile{
		Version:       doc.Version,
		ValidatedAt:   doc.ValidatedAt,
		Gate:          doc.Gate,
		EngineVersion: doc.EngineVersion,
		Components:    make(map[string]LockfileComponent, len(doc.Components)),
	}
	for name, c := range doc.Components {
		lf.Components[name] = LockfileComponent{
			Repo:               c.Repo,
			Digest:             c.Digest,
			BuiltAgainstEngine: c.BuiltAgainstEngine,
		}
	}
	return lf, nil
}
