package memql

// seed_concept_validator.go -- a seed's signature concept, held to the concept
// registry at load (memql#5433).
//
// A seed writes its row through create<Concept>, the mutation named after the
// concept its signature binds (`seed site os { ... }` writes through
// createSite), and reads the row back through that concept's canonical id
// (canonicalSeedConceptID) for the freshness check and the per-user dedup
// guard. A seed whose concept names NO registered concept loaded clean and
// failed at materialization instead -- on every boot, as an error line in the
// log, with the row never written and nothing refused. That is the seed half
// of the defect memql#5433 fixes for a query and a mutation signature, and it
// is refused the same way: at load, with the same code.
//
// THE RULE IS THE MATERIALIZER'S, deliberately. A query or a mutation
// signature is resolved through the file's imports and the domain's ambient
// scope (ConceptResolver.ResolveSignatureConceptInNamespace), because the
// loader rewrites its AST with the id it resolves. A seed's binding is read by
// the materializer, which resolves the bare name across every mounted domain
// and never consults an import. Holding a seed to the stricter rule would
// refuse seeds that materialize -- dsl/skills/seeds/ binds `skill` from a
// nested directory with no import, and boots -- and holding it to anything
// laxer would pass seeds that cannot. So the check asks what the materializer
// needs: that SOME mounted domain declares a concept of that name.
//
// An AMBIGUOUS name is not refused here. The write goes through
// create<Concept>, which resolves on its own, so such a seed still
// materializes; what an ambiguous name costs is the freshness check (every
// boot writes a new version) and the per-user dedup lookup, which the
// materializer already reports loudly when it happens.

import (
	"fmt"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// seedBindingViolation is one seed whose signature concept no mounted domain
// declares.
type seedBindingViolation struct {
	Seed string
	// File is the seed's source file, the path the load report names.
	File string
	Err  *SignatureConceptError
}

// validateSeedConceptBindings reports every registered seed whose signature
// concept no registered concept carries as its name, in seed-name order so
// the boot log and the strict-boot refusal are deterministic.
func validateSeedConceptBindings(seeds *SeedRegistry, concepts memoryNodes.Registry) []seedBindingViolation {
	if seeds == nil || concepts == nil {
		return nil
	}
	resolver := NewConceptResolver(concepts)
	all := seeds.All()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })

	var out []seedBindingViolation
	for _, def := range all {
		if def == nil || strings.TrimSpace(def.UseConcept) == "" {
			continue // compileSeedDecl refuses a seed with no binding
		}
		if len(resolver.conceptCandidates(def.UseConcept)) > 0 {
			continue
		}
		out = append(out, seedBindingViolation{
			Seed: def.Name,
			File: seedOriginFile(def),
			Err: &SignatureConceptError{
				Name: def.UseConcept,
				Reason: fmt.Sprintf("no mounted domain declares a concept %q, and the seed writes its row through create%s, which would fail on every boot",
					def.UseConcept, ucFirst(def.UseConcept)),
				Fix: "declare the concept, or bind the seed to one that is declared",
			},
		})
	}
	return out
}

// seedOriginFile is the source file a seed was declared in: its Origin,
// "unified:<path>:<name>", without the decoration.
func seedOriginFile(def *SeedDefinition) string {
	file := strings.TrimPrefix(def.Origin, "unified:")
	return strings.TrimSuffix(file, ":"+def.Name)
}
