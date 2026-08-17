package genesis

// findmissing.go -- the seal floor's presence check (memql#3963).
//
// These two functions live on the ENVELOPE side of the split rather than in
// component/envregistry, and the reason is that their only caller is Seal.
// They answer "does this developer's .env cover the strict superset
// genesis-seal demands before it will produce an envelope", which is a
// question about sealing and about nothing else -- the registry's own
// consumers (boot validation, first-deploy injection, the envscan drift gate)
// each ask a different one.
//
// So they leave with the envelope in memql#3966 rather than surviving in the
// registry as two exported functions nothing calls.

import "github.com/znasllc-io/memql/component/envregistry"

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
		if !envregistry.PresentWithLegacy(have, name) {
			missing = append(missing, name)
		}
	}
	return missing
}
