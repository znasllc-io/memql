package knowledge

import (
	"fmt"
	"sync"
)

// registry.go is the generic product seam for pack-registered knowledge seed
// domains. It mirrors the semantics of memql.RegisterSuggestDomain (the
// AiSuggest domain registry): the engine ships its own catalog
// (standardDomains) and a product pack folds ITS domains + seed corpora in at
// init() time via RegisterSeedDomain. An engine binary built without a pack has
// an empty registry -- allSeedDomains() then returns only the engine catalog.
//
// This exists so the engine carries zero product-specific seed data. A
// product pack (e.g. the product's UI domain + its corpus) registers its
// domains from its own knowledge_seed.go at init() time.

// SeedCorpusEntry is one document-chunk source in a seed corpus. The pair
// (SourceRef, Text) feeds the idempotent chunk id sha256(domainId + sourceRef +
// seq + text) via chunkIdFor, so both fields are effectively wire bytes: any
// drift re-hashes the chunk id and forces a re-embed on the next boot.
type SeedCorpusEntry struct {
	SourceRef string
	Text      string
}

// SeedDomainRegistration is a product-registered knowledge domain plus its
// optional seed corpus, folded into the startup catalog seeder. Domain is the
// same StandardDomain shape the engine catalog uses; Corpus is ingested into
// the domain exactly like the engine's own seed corpora.
type SeedDomainRegistration struct {
	Domain StandardDomain
	Corpus []SeedCorpusEntry
}

var (
	seedDomainMu   sync.Mutex
	seedDomainRegs []SeedDomainRegistration
	seedDomainIDs  = map[string]bool{}
)

// RegisterSeedDomain folds a product knowledge domain + optional seed corpus
// into the engine startup catalog seeder. Called from a pack's init(). Panics
// (fail-loud at startup, never a silent mis-seed) on:
//   - an empty Domain.ID,
//   - a collision with an engine standardDomains catalog id, or
//   - a duplicate registration of an id already registered here.
//
// Mirrors memql.RegisterSuggestDomain: the engine owns its catalog; a pack owns
// its additions. Engine binaries without a pack never call this, so the
// registry stays empty.
func RegisterSeedDomain(reg SeedDomainRegistration) {
	id := reg.Domain.ID
	if id == "" {
		panic("knowledge.RegisterSeedDomain: registration has an empty Domain.ID")
	}
	seedDomainMu.Lock()
	defer seedDomainMu.Unlock()
	for i := range standardDomains {
		if standardDomains[i].ID == id {
			panic(fmt.Sprintf("knowledge.RegisterSeedDomain: id %q collides with the engine standardDomains catalog", id))
		}
	}
	if seedDomainIDs[id] {
		panic(fmt.Sprintf("knowledge.RegisterSeedDomain: id %q already registered", id))
	}
	seedDomainIDs[id] = true
	seedDomainRegs = append(seedDomainRegs, reg)
}

// RegisteredSeedDomains returns the pack-registered seed domains in
// registration order. The returned slice is a copy, so the caller can retain or
// mutate it without racing the registry.
func RegisteredSeedDomains() []SeedDomainRegistration {
	seedDomainMu.Lock()
	defer seedDomainMu.Unlock()
	out := make([]SeedDomainRegistration, len(seedDomainRegs))
	copy(out, seedDomainRegs)
	return out
}

// allSeedDomains is the single catalog accessor used by the seeder,
// content-seeder, catalog lookups, and bridge resolution. It returns the engine
// standardDomains catalog followed by every pack-registered Domain, in
// registration order. Callers must not assume the engine entries stay at fixed
// indices across builds.
func allSeedDomains() []StandardDomain {
	regs := RegisteredSeedDomains()
	out := make([]StandardDomain, 0, len(standardDomains)+len(regs))
	out = append(out, standardDomains...)
	for _, reg := range regs {
		out = append(out, reg.Domain)
	}
	return out
}
