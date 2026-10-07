package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// StandardDomain is a seed record for a knowledge domain shipped with
// MemQL. Seeding is idempotent: the seedStandardDomains capability only
// creates domains that don't already exist, so re-running on every
// startup is safe and manual overrides (admin-edited domain rows) are
// preserved.
type StandardDomain struct {
	ID                  string
	Name                string
	Description         string
	Category            string
	RelevantForRoles    []string
	RequiredByToolSlugs []string
	// Source distinguishes how the domain's chunks get cited at chat
	// time. "" (the default) => llmSeeded: subject-matter expertise cited
	// as "your X training" in agent replies. "appStructure" =>
	// operator/internal documentation whose chunks shape the agent's
	// behavior and are NOT audibly cited (see the citation registry in
	// integrations/agent/replier.go appStructureDomainIds).
	Source string
	// Tier drives the seeder's content strategy per
	// docs/internal/planning/knowledge-seeder.md:
	//   "A" -- general knowledge, LLM-generated chunks ship as-is.
	//   "B" -- safety-relevant; LLM-generated + a disclaimer chunk
	//          ("general info, not professional advice") prepended.
	//   "C" -- high-stakes specialist (clinical medicine, surgical
	//          technique, securities advice, legal practice). Don't
	//          auto-seed; the seeder writes a single placeholder
	//          chunk telling the user to upload their own
	//          authoritative content. Empty string defaults to "A"
	//          for backwards-compat with domains added before this
	//          field landed.
	Tier string
	// BroadSurvey marks a domain whose scope spans many sub-areas
	// (multi-millennium history, multi-civilization cultural studies,
	// multi-discipline philosophy, etc.). The default 30-chunk target
	// produces ~5 chunks per major sub-area for these, which is too
	// thin to surface specific named events / works / figures by
	// retrieval. Survey domains get a 60-chunk target plus a tighter
	// prompt that requires named-anchor coverage. Narrow domains
	// (e.g. "Heisenberg uncertainty principle" lives inside
	// quantum-mechanics) keep the 30 default.
	BroadSurvey bool
}

// DomainTier exposes the tier discriminator for non-Go consumers
// (kept as a string column on the concept payload so SQL can filter).
type DomainTier string

const (
	TierA DomainTier = "A"
	TierB DomainTier = "B"
	TierC DomainTier = "C"
)

// WikipediaArticles is the optional set of Wikipedia article titles
// to fetch + chunk + embed when the seeder runs against a Tier C
// domain. Set on Tier C entries so the seeder produces real
// authoritative content (with attribution) instead of the
// placeholder chunk. Tier A + B domains ignore this field.
//
// Lives as a separate map (not on StandardDomain itself) so the
// expansion entries above can stay terse -- WikipediaArticles
// only exists for Tier C entries that have a curated mapping.
// Empty mapping (or domain absent from the map) => Tier C falls
// back to the placeholder chunk.
var tierCWikipediaArticles = shippedSeedCatalog().Wikipedia

// wikipediaArticlesFor returns the Tier C Wikipedia mapping for a
// domain id, or nil if none configured. Used by runSeederForDomain
// to decide whether to fetch real content or write the placeholder.
func wikipediaArticlesFor(domainId string) []string {
	return tierCWikipediaArticles[domainId]
}

// standardDomains is the engine-shipped knowledge-domain catalog. We seed
// it into the database on startup so any client can query
// v1:knowledge:knowledgeDomain instead of carrying the list in-bundle. The
// RelevantForRoles slice drives the role picker: a domain whose
// RelevantForRoles contains role "X" surfaces in role X's picker.
//
// This catalog is engine-generic -- product-specific domains + their seed
// corpora register from a pack via RegisterSeedDomain (see registry.go) and
// are merged in by allSeedDomains(); the engine never hardcodes them here.
var standardDomains = shippedSeedCatalog().Domains

// tierOverride patches the tier of legacy domains (those added before
// the Tier field landed) without rewriting every entry in
// standardDomains. Keys are domain IDs; values are the explicit tier
// to use. Anything not in this map AND not carrying an explicit Tier
// in standardDomains defaults to "A" via effectiveTier().
//
// Tier-B (safety-relevant -- LLM-seeded but with a disclaimer chunk
// prepended): finance, taxes, legal, mental health, medical-records,
// parenting, child-development, dietary-restrictions, labor-law.
//
// Tier-C (high-stakes specialist -- not auto-seeded): currently none
// in the legacy 96; the new medical-clinical and surgical entries
// in the catalog expansion above are tagged C explicitly.
var tierOverride = shippedSeedCatalog().Tiers

// effectiveTier returns the tier the seeder should use for a domain.
// Explicit Tier on the StandardDomain wins; tierOverride map is the
// next layer; default is "A". Returns one of "A" / "B" / "C".
func effectiveTier(d StandardDomain) string {
	if t := strings.TrimSpace(d.Tier); t != "" {
		return t
	}
	if t, ok := tierOverride[d.ID]; ok {
		return t
	}
	return "A"
}

// roleDomainMap mirrors the old ROLE_DOMAIN_MAP. The domain's RelevantForRoles
// field gets populated from this inverted mapping at seed time so a role
// -> domains lookup is a single query against the domain concept instead
// of a separate mapping concept.
var roleDomainMap = shippedSeedCatalog().Roles

// computerUseSeedCorpus is the operational manual for the
// `computer-use` capability. A SeedCorpusEntry list (sourceRef + text)
// ingested through the seedStandardDomains handler. Each
// chunk is a self-contained paragraph anchored on a clear topic so
// RAG retrieval can pull the right chunk for a given user query
// without needing the full set in context.
//
// Authoring rules for adding chunks here (read before edits):
//   - Keep each chunk under ~2 KB. RAG ranks chunks individually;
//     fat chunks dilute relevance.
//   - Lead with the topic anchor in the first sentence (e.g.
//     "Scope tiers determine what a Computer Use call can DO ...").
//     The retriever embeds the whole chunk; the lead sentence
//     is what gives it semantic shape.
//   - Tool-name references stay verbatim: `workerHost`,
//     `workerComputer`, `workerStatus`, `requestComputerUseScope`.
//     The agent learns the wire-level names from tool definitions;
//     the chunk reinforces when to reach for each one.
//   - NO hardcoded user-task examples ("open Safari", "list
//     Downloads"). Pattern shape only. If a specific agent needs
//     curated examples, those land in a per-agent training source,
//     not in the standard seed.
var computerUseSeedCorpus = shippedSeedCatalog().ComputerUse

// workbenchSeedCorpus is the operational manual for the `workbench-use`
// capability. Universal capability (every agent has it by default).
// Same shape as computerUseSeedCorpus -- chunks lead with a topic
// anchor and stay self-contained so RAG retrieval lands the right
// chunk for a given query.
var workbenchSeedCorpus = shippedSeedCatalog().Workbench

// recentChatSeedCorpus is the operational manual for the single-chat
// architecture (one v1:cognition:utterance stream per space) plus the
// assistant/specialist split. Ingested into the recent-chat knowledge
// domain at startup. Each chunk is intentionally short and self-
// contained -- RAG retrieval surfaces the chunk closest to the agent's
// current question, so a single chunk should be readable in isolation.
//
// Authoring rule: use the umbrella tool name ("recentChat") in user-
// facing language. Never expose the operations as standalone tool
// names -- they are arguments to the umbrella tool, not separate
// tools.
var recentChatSeedCorpus = shippedSeedCatalog().RecentChat

// retiredSeedCorpusPairs lists (domainId, sourceRef) corpus pairs that were
// REMOVED from the seed catalog (entry relocated to another domain or
// deleted outright). seedStandardDomainsHandler purges them on every run so
// already-deployed databases don't keep serving the stale chunk forever;
// keep a pair here until every environment has run the purge at least once.
var retiredSeedCorpusPairs = shippedSeedCatalog().Retired

// seedStandardDomainsHandler creates the shipped knowledge domains
// (engine catalog + any pack-registered seed domains) and ingests their
// seed corpora. Idempotent: skips any domain whose id is already present;
// re-ingests corpus chunks whose content has changed (different text ->
// different chunk id).
func (i *Integration) seedStandardDomainsHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.engine == nil {
		return nil, fmt.Errorf("knowledge.seedStandardDomains: engine not configured")
	}

	domains := map[string]StandardDomain{}
	facts := []any{}
	for _, d := range allSeedDomains() {
		if d.RelevantForRoles == nil {
			d.RelevantForRoles = rolesForDomain(d.ID)
		}
		domains[d.ID] = d
		facts = append(facts, map[string]any{"id": d.ID, "source": d.Source, "category": d.Category, "tier": effectiveTier(d)})
	}
	corpora := []any{}
	entries := map[string]SeedCorpusEntry{}
	entryDomains := map[string]string{}
	addCorpus := func(domain string, values []SeedCorpusEntry) {
		for _, entry := range values {
			key := chunkIdFor(domain, entry.SourceRef, 0, entry.Text)
			entries[key], entryDomains[key] = entry, domain
			corpora = append(corpora, map[string]any{"key": key})
		}
	}
	addCorpus("computer-use", computerUseSeedCorpus)
	addCorpus("workbench", workbenchSeedCorpus)
	addCorpus("recent-chat", recentChatSeedCorpus)
	addCorpus("work-guidance", workGuidanceCorpus)
	for _, reg := range RegisteredSeedDomains() {
		addCorpus(reg.Domain.ID, reg.Corpus)
	}
	retired := []any{}
	for _, pair := range retiredSeedCorpusPairs {
		retired = append(retired, map[string]any{"domainId": pair.DomainID, "sourceRef": pair.SourceRef})
	}
	created, ingested := 0, 0
	_, err := workflowhost.Run(ctx, "knowledgeSeedCatalogWorkflow", map[string]any{"domains": facts, "corpora": corpora, "retired": retired, "forceIngest": args["forceIngest"] == true}, workflowhost.Options{Logger: i.Logger, Operations: map[string]workflowhost.Operation{
		"knowledgeCatalogDomainExists": func(ctx context.Context, a map[string]any) (any, error) {
			return i.domainExists(ctx, stringArg(a, "domainId")), nil
		},
		"knowledgeCreateCatalogDomain": func(ctx context.Context, a map[string]any) (any, error) {
			d, ok := domains[stringArg(a, "domainId")]
			if !ok {
				return nil, fmt.Errorf("domain is outside the seed catalog")
			}
			query := fmt.Sprintf(`mutation createKnowledgeDomain(domainId: %s, name: %s, description: %s, category: %s, relevantForRoles: %s, requiredByToolSlugs: %s, active: true, tier: %s, source: %s, predefined: true)`, quoteString(d.ID), quoteString(d.Name), quoteString(d.Description), quoteString(stringArg(a, "category")), jsonArray(d.RelevantForRoles), jsonArray(d.RequiredByToolSlugs), quoteString(stringArg(a, "tier")), quoteString(stringArg(a, "source")))
			_, err := i.engine.Execute(ctx, query)
			if err == nil {
				created++
			}
			return nil, err
		},
		"knowledgeSeedCorpusExists": func(ctx context.Context, a map[string]any) (any, error) {
			return i.chunkExistsById(ctx, stringArg(a, "key")), nil
		},
		"knowledgePurgeSeedCorpus": func(ctx context.Context, a map[string]any) (any, error) {
			key := stringArg(a, "key")
			entry, ok := entries[key]
			if !ok {
				return nil, fmt.Errorf("corpus is outside the seed catalog")
			}
			return nil, i.purgeChunksForSource(ctx, entryDomains[key], entry.SourceRef)
		},
		"knowledgeIngestSeedCorpus": func(ctx context.Context, a map[string]any) (any, error) {
			key := stringArg(a, "key")
			entry, ok := entries[key]
			if !ok {
				return nil, fmt.Errorf("corpus is outside the seed catalog")
			}
			_, err := i.ingestHandler(ctx, map[string]any{"domainId": entryDomains[key], "text": entry.Text, "source": stringArg(a, "source"), "sourceRef": entry.SourceRef}, 0)
			if err == nil {
				ingested++
			}
			return nil, err
		},
		"knowledgePurgeRetiredCorpus": func(ctx context.Context, a map[string]any) (any, error) {
			for _, pair := range retiredSeedCorpusPairs {
				if pair.DomainID == stringArg(a, "domainId") && pair.SourceRef == stringArg(a, "sourceRef") {
					return nil, i.purgeChunksForSource(ctx, pair.DomainID, pair.SourceRef)
				}
			}
			return nil, fmt.Errorf("corpus is not retired")
		},
	}})
	i.Logger.Info("knowledge.seedStandardDomains: complete", "domainsCreated", created, "corpusIngested", ingested, "error", err)
	return nil, err
}

// domainExists returns true if a knowledge-domain row with the given
// id is already present. Uses direct SQL rather than going through
// engine.Execute + shape parsing because the seed path runs at every
// startup and needs to be cheap + unambiguous.
//
// staged-data: MUST-NOT-GATE -- SEED IDEMPOTENCY, and it runs at EVERY startup
// (epic memql#3974, task memql#3984).
//
// The whole purpose of this read is "do not write this again". Hide a staged
// row and the answer is false, the seed writes a duplicate, and it does so on
// every boot for as long as the concept stays staged. For the chunk-level
// siblings below the same loop also re-EMBEDS what it re-creates, so the cost
// is a recurring vendor bill on top of the duplicate rows.
//
// This ruling covers chunkExistsForSource, purgeChunksForSource and
// chunkExistsById below, which are the same seed pass.
func (i *Integration) domainExists(ctx context.Context, domainId string) bool {
	if i.db() == nil {
		return false
	}
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:knowledgeDomain'
		  AND (payload->>'active' = 'true' OR payload->>'active' IS NULL)
		  AND id = $1
	`
	canonicalId := id.BuildNodeId("v1:knowledge:knowledgeDomain", domainId)
	if err := i.db().QueryRowContext(ctx, sqlText, canonicalId).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// chunkExistsForSource returns true if at least one chunk row exists
// for the given domain+sourceRef. Uses direct SQL because we don't have
// a .memql query for this lookup and it's a ~microsecond check.
//
// staged-data: MUST-NOT-GATE -- see domainExists. Gated, every startup
// re-ingests and re-embeds the same corpus.
func (i *Integration) chunkExistsForSource(ctx context.Context, domainId, sourceRef string) bool {
	if i.db() == nil {
		return false
	}
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:documentChunk'
		  AND (payload->>'domainId') = $1
		  AND (payload->>'sourceRef') = $2
	`
	if err := i.db().QueryRowContext(ctx, sqlText, domainId, sourceRef).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// purgeChunksForSource hard-deletes every v1:knowledge:documentChunk
// row (and its node_vectors row) for a given (domain, sourceRef)
// pair. Called from the seed right before
// a re-ingest when a text change is detected, so the new version is
// the only live copy for its sourceRef. Direct SQL (not a DSL
// mutation) because seeds run before automations are scheduled and
// we're already doing direct SQL for the sibling lookups.
//
// staged-data: MUST-NOT-GATE -- gating the SELECT subquery inside the
// node_vectors DELETE STRANDS VECTORS, and stranded vectors keep serving
// deleted content (epic memql#3974, task memql#3984).
//
// The two statements are a pair: the first deletes the embeddings for a
// (domain, sourceRef), the second deletes the chunks themselves. Gate only the
// subquery in the first and the chunk rows still go, while their node_vectors
// rows stay -- and a dangling vector still satisfies the JOIN in similarTo /
// findSimilar / recall, so text that was deleted keeps being retrieved into
// agent context. The gate meant to withhold content would be the reason
// deleted content survives.
//
// The second statement is a DELETE and so is a write, outside this tier's
// subject (memql#3985) -- it is classified here because it lives in the same
// function and the pair only makes sense ruled on together.
func (i *Integration) purgeChunksForSource(ctx context.Context, domainId, sourceRef string) error {
	if i.db() == nil {
		return nil
	}
	// Delete embeddings first so we never leave node_vectors rows
	// dangling against a missing MemoryNodes chunk. The subquery
	// picks up every chunk id matching the (domain, sourceRef) pair
	// regardless of version/text hash.
	vecSQL := `
		DELETE FROM %s
		WHERE id IN (
		    SELECT id FROM "MemoryNodes"
		    WHERE concept = 'v1:knowledge:documentChunk'
		      AND (payload->>'domainId') = $1
		      AND (payload->>'sourceRef') = $2
		)
	`
	tables, err := memql.EmbeddingVectorTables(ctx, i.db())
	if err != nil {
		return err
	}
	for _, table := range tables {
		if _, err := i.db().ExecContext(ctx, fmt.Sprintf(vecSQL, table), domainId, sourceRef); err != nil {
			return fmt.Errorf("delete vectors: %w", err)
		}
	}
	chunkSQL := `
		DELETE FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:documentChunk'
		  AND (payload->>'domainId') = $1
		  AND (payload->>'sourceRef') = $2
	`
	if _, err := i.db().ExecContext(ctx, chunkSQL, domainId, sourceRef); err != nil {
		return fmt.Errorf("delete MemoryNodes: %w", err)
	}
	return nil
}

// chunkExistsById returns true if a chunk row with the given chunk id
// already exists. Used by the seed to skip re-ingesting identical
// content while still allowing source-text edits to flow through: if
// a seed corpus text changes, chunkIdFor produces a
// different hash that won't match, and the chunk is ingested fresh.
// chunkIdFor returns the BARE hash; the stored row id is the
// concept-qualified form the engine composes at insert
// (v1:knowledge:documentChunk:<hash>), so match on that.
//
// staged-data: MUST-NOT-GATE -- see domainExists. This is the per-chunk
// idempotency check; gated, it re-ingests and re-embeds every seed chunk on
// every boot.
func (i *Integration) chunkExistsById(ctx context.Context, chunkId string) bool {
	if i.db() == nil {
		return false
	}
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:documentChunk'
		  AND id = $1
	`
	canonicalId := id.BuildNodeId("v1:knowledge:documentChunk", chunkId)
	if err := i.db().QueryRowContext(ctx, sqlText, canonicalId).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// rolesForDomain inverts roleDomainMap to get the list of role slugs
// that should see a given domain in their picker.
func rolesForDomain(domainId string) []string {
	if domainId == "business-administration" {
		// business-administration is the always-visible catalog
		// baseline -- empty role list means "show in every picker".
		return []string{}
	}
	var roles []string
	for role, list := range roleDomainMap {
		for _, id := range list {
			if id == domainId {
				roles = append(roles, role)
				break
			}
		}
	}
	return roles
}

func hasResult(result any) bool {
	if result == nil {
		return false
	}
	// The engine wraps shape output in an ExecuteResult; we just look
	// for any non-empty collection or non-nil node. JSON-round-trip to
	// normalise then inspect.
	raw, err := json.Marshal(result)
	if err != nil {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	// Common empty shapes: "[]", "null", "{}".
	switch trimmed {
	case "", "null", "[]", "{}", "[null]", `[{}]`:
		return false
	}
	// If it's an array, make sure it has at least one populated entry.
	var arr []any
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, item := range arr {
			if item == nil {
				continue
			}
			if m, ok := item.(map[string]any); ok && len(m) == 0 {
				continue
			}
			return true
		}
		return false
	}
	return true
}

func coalesceCategory(c string) string {
	if c == "" {
		return "business"
	}
	return c
}

func jsonArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	out, _ := json.Marshal(items)
	return string(out)
}
