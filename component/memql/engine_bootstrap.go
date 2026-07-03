package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/actions"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

func (e *MemQLEngine) Init(concepts concept.Registry) error {
	e.relationships = relationshipRegistry{}
	e.concepts = nil
	e.initialized = false
	e.prompts = nil
	e.providers = nil
	e.functions = nil
	e.tools = nil
	e.aiRuntime = nil

	// Fresh structured load report for this Init pass (epic #2351 / S2,
	// memql#2357). Every unified loader below accumulates its skipped[]
	// constructs into it; the S5 uniqueness gate folds in duplicates; the
	// strict-boot check consults it before the tail summary log. Rebuilt
	// per Init so tests that re-Init never leak a stale problem set.
	report := newLoadReport()
	e.loadReport = report

	if concepts == nil {
		return fmt.Errorf("concept registry is required")
	}

	all := concepts.List()
	if len(all) == 0 {
		e.relationships = relationshipRegistry{ByConcept: make(map[string][]RelationshipDefinition)}
		e.concepts = concepts
		e.initialized = true
		return nil
	}

	conceptNames := make(map[string]struct{}, len(all))
	relationshipsByConcept := make(map[string][]RelationshipDefinition, len(all))

	for _, c := range all {
		if c == nil {
			return fmt.Errorf("concept registry contains nil concept entry")
		}

		name := strings.TrimSpace(c.Name)
		if name == "" {
			return fmt.Errorf("concept registry contains a concept with empty name")
		}

		if _, exists := conceptNames[name]; exists {
			return fmt.Errorf("concept registry contains duplicate concept %q", name)
		}

		c.Name = name
		conceptNames[name] = struct{}{}
		relationshipsByConcept[name] = nil
	}

	duplicateTracker := make(map[string]map[string]map[string]struct{})
	reverseTracker := make(map[string]relationshipEdge)

	for _, c := range all {
		name := c.Name
		if len(c.Relationships) == 0 {
			continue
		}

		for idx := range c.Relationships {
			normalized, err := normalizeRelationshipDefinition(c.Relationships[idx])
			if err != nil {
				return fmt.Errorf("concept %q relationship[%d]: %w", name, idx, err)
			}

			if _, ok := conceptNames[normalized.TargetConcept]; !ok {
				// With @visibility filtering, the target concept may exist
				// on another node type. Skip validation for targets not in
				// the local registry — the relationship is structurally
				// valid, just not resolvable on this node.
				e.Logger.Debug("relationship target not in local registry (filtered by visibility)",
					"concept", name,
					"target", normalized.TargetConcept,
				)
				continue
			}

			if err := checkDuplicateRelationship(duplicateTracker, name, normalized); err != nil {
				return err
			}

			if err := checkRelationshipDirection(reverseTracker, name, normalized); err != nil {
				return err
			}

			c.Relationships[idx] = normalized
			relationshipsByConcept[name] = append(relationshipsByConcept[name], normalized)
		}
	}

	for _, c := range all {
		nodeType := strings.ToLower(strings.TrimSpace(c.NodeType))
		if nodeType == "" {
			nodeType = concept.NodeTypeObject
		}

		switch nodeType {
		case concept.NodeTypeObject, concept.NodeTypeCollection, concept.NodeTypeReference:
		default:
			return fmt.Errorf("concept %q has invalid node type %q", c.Name, c.NodeType)
		}

		c.NodeType = nodeType

		defs := relationshipsByConcept[c.Name]

		if nodeType == concept.NodeTypeReference && !hasRelationshipOfType(defs, relationshipTypeAlias, relationshipTypeEquals) {
			return fmt.Errorf("reference concept %q must declare alias or equals relationship", c.Name)
		}

		if nodeType == concept.NodeTypeCollection && !hasRelationshipOfType(defs, relationshipTypeContains) {
			return fmt.Errorf("collection concept %q must declare contains relationship", c.Name)
		}

		for _, def := range defs {
			switch def.Type {
			case relationshipTypeContains:
				if nodeType != concept.NodeTypeCollection {
					return fmt.Errorf("concept %q must be type collection to define contains relationships", c.Name)
				}
			}
		}
	}

	e.relationships = relationshipRegistry{ByConcept: relationshipsByConcept}
	e.concepts = concepts

	schemaIdx, err := buildSchemaIndex(concepts)
	if err != nil {
		return err
	}
	specRegistry, err := loadEmbeddedSpecs(e.Logger, schemaIdx)
	if err != nil {
		return err
	}
	e.schemaIdx = schemaIdx
	e.specs = specRegistry

	// Load functions after specs (functions can reference specs)
	functionRegistry, err := loadEmbeddedFunctions(e.Logger, e.concepts)
	if err != nil {
		return err
	}
	e.functions = functionRegistry

	// Pass 2 of the DSL restructure migration: load functions from
	// the unified domain-first tree (dsl/<domain>/<entity>.memql)
	// and upsert them into the same registry. During the
	// transitional state both loaders run; the unified loader
	// overwrites each function's registry entry with the entry
	// parsed from the consolidated tree. Functions only in the
	// legacy tree keep working via the legacy path; functions only
	// in the new tree (none today) get registered exclusively via
	// this loader. When the legacy tree is retired (Pass 3), the
	// loadEmbeddedFunctions call above goes away.
	if _, _, ulErr := LoadUnifiedFunctions(e.Logger, functionRegistry, e.concepts, report); ulErr != nil {
		e.Logger.Warn("unified function loader returned an error; legacy loader covers the gap",
			"component", "memql.engine",
			"error", ulErr)
	}

	if err := e.initBuiltinExecutorHandlers(); err != nil {
		return err
	}

	// Load shape templates
	shapeRegistry, err := loadEmbeddedShapes(e.Logger, e.concepts)
	if err != nil {
		return err
	}
	e.shapes = shapeRegistry

	// Pass 2 of the DSL restructure: overlay shapes from the new
	// domain-first tree.
	if _, ulErr := LoadUnifiedShapes(e.Logger, shapeRegistry, report); ulErr != nil {
		e.Logger.Warn("unified shape loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}

	// C5 (memql#2035): expand empty-body, signature-bound shapes into a
	// projection over every projectable (non-@internal) field of their
	// concept. Runs after both concepts and shapes are loaded so the
	// concept field list is available. Additive -- shapes with an
	// explicit body are untouched.
	expandDefaultShapeProjections(e.Logger, shapeRegistry, e.concepts)

	// Load specs + traits from the new tree via the dedicated parser.
	// Specs / traits use the SpecRegistry; the unified function
	// loader does NOT handle them. Without this call the registry
	// stays empty + every executor lookup misses.
	if _, ulErr := LoadUnifiedSpecs(e.Logger, specRegistry, report); ulErr != nil {
		e.Logger.Warn("unified spec loader returned an error",
			"component", "memql.engine", "error", ulErr)
	}

	// Spec/shape binding redesign (epic #2281): finalize every spec +
	// trait now that concepts + shapes are loaded. Resolves each spec's
	// signature binding, rewrites the body's bare field references to
	// their underlying access form, and classifies row-spec vs
	// context-spec. A binding violation is a contract error -- surfaced
	// loudly rather than silently dropped.
	if rsErr := resolveSpecBindings(e.Logger, specRegistry, shapeRegistry, e.concepts); rsErr != nil {
		e.Logger.Warn("spec binding resolver reported violations",
			"component", "memql.engine", "error", rsErr)
	}

	// Pass 2: overlay builtins from the new tree (struct-form
	// `builtin NAME { ... }` blocks). Builtins are functions
	// internally so they merge into the FunctionRegistry.
	if _, ulErr := LoadUnifiedBuiltins(e.Logger, functionRegistry, report); ulErr != nil {
		e.Logger.Warn("unified builtin loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}

	// Load MCP tools
	toolRegistry, err := loadToolRegistry(e.Logger)
	if err != nil {
		return err
	}

	// Pass 2: overlay tools from the new tree.
	if _, ulErr := LoadUnifiedTools(e.Logger, toolRegistry, report); ulErr != nil {
		e.Logger.Warn("unified tool loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}

	// Auto-register enabled MemQL functions as MCP tools (Option A).
	// This exposes functions to tool-calling agents without allowing free-form MemQL.
	registerFunctionTools(e.Logger, functionRegistry, toolRegistry)

	e.tools = toolRegistry

	// Boot self-check (memql#1156): a capability tool missing from the
	// registry means a tool slice silently failed to load (the loader
	// logs+skips unparseable slices). Surface it LOUDLY here at boot instead
	// of letting it resurface at runtime as "tool not in registry" -- which is
	// what stranded produceArtifact and drove a plan/event storm (memql#1153).
	VerifyCapabilityToolsRegistered(toolRegistry.Has, e.Logger)

	promptRegistry, err := loadPromptRegistry(e.Logger)
	if err != nil {
		return err
	}

	// Pass 2: overlay prompts from the new tree. The unified loader
	// resolves @templateFile references against dsl.Tree() so the
	// .tmpl sidecars in dsl/<domain>/prompts/ are picked up.
	// Reuse the same partials the legacy loader built.
	if partials, pErr := loadPartials(); pErr == nil && partials != nil {
		if _, ulErr := LoadUnifiedPrompts(e.Logger, promptRegistry, partials, report); ulErr != nil {
			e.Logger.Warn("unified prompt loader returned an error; legacy covers gap",
				"component", "memql.engine", "error", ulErr)
		}
	}

	// AgentRegistry is now row-backed (populated by
	// AgentRegistry.LoadFromRows at the end of the SeedMaterializer's
	// startup sweep -- see app/engine.go). The legacy
	// LoadUnifiedAgents path that walked `agent X { }` declarations
	// has been removed; every platform agent is a `seed X { }`
	// declaration in dsl/agents/ and the materialized v1:agents:agent
	// rows are the registry's source of truth.
	agentRegistry := NewAgentRegistry()
	e.agents = agentRegistry

	// Load DSL-declared seeds. Walks every `seed NAME { ... }` block
	// across the DSL tree, resolves @templateFile sidecars, and
	// registers compiled SeedDefinitions in the SeedRegistry. The
	// SeedMaterializer (follow-up commit) reads from this registry on
	// engine startup + on v1:identity:user create events to materialize
	// rows into v1:agents:agent and friends.
	seedRegistry := NewSeedRegistry()
	if _, slErr := LoadUnifiedSeeds(e.Logger, seedRegistry, report); slErr != nil {
		e.Logger.Warn("unified seed loader returned an error",
			"component", "memql.engine", "error", slErr)
	}
	e.seeds = seedRegistry
	e.seedMaterializer = NewSeedMaterializer(e, e.seeds)

	// Wire concept-storage resolvers BEFORE loading providers so a
	// provider's auth.apiKey="${MEMQL_AI_OPENAI_API_KEY}" picks up the
	// value from v1:platform:globalSecret if a developer has run
	// `make secrets-seed`. OS env stays as a legacy fallback per the
	// Phase 4b transition. Calling these is idempotent; they overwrite
	// any prior installation, which is the desired behavior in tests.
	SetSystemSecretResolver(func(ctx context.Context, name string) (string, error) {
		return e.ResolveSystemSecret(ctx, name)
	})
	SetSystemVariableResolver(func(ctx context.Context, name string) (string, error) {
		return e.ResolveSystemVariable(ctx, name)
	})

	providerRegistry, err := loadAIProviders(e.Logger)
	if err != nil {
		return err
	}

	// Pass 2: overlay providers from the new tree. dsl/providers/
	// has one file per vendor with the base + all models in one
	// place; each `provider NAME { ... }` block becomes a registry
	// entry.
	if _, ulErr := LoadUnifiedProviders(e.Logger, providerRegistry, report); ulErr != nil {
		e.Logger.Warn("unified provider loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}
	policyRegistry, err := loadAIPolicies(e.Logger)
	if err != nil {
		return err
	}
	if _, ulErr := LoadUnifiedPolicies(e.Logger, policyRegistry, report); ulErr != nil {
		e.Logger.Warn("unified policy loader returned an error; legacy stub covers gap",
			"component", "memql.engine", "error", ulErr)
	}
	e.prompts = promptRegistry
	e.providers = providerRegistry
	e.policies = policyRegistry
	e.aiRuntime = newAIRuntime(e.Logger, promptRegistry, providerRegistry, e.aiCacheConfig)

	// ReloadAIProviders is exposed below so dev workflows that wipe
	// the database before re-seeding (make dev-refresh) can refresh
	// provider auth resolution AFTER seeding completes -- without
	// this hook, providers eager-load before secrets exist in
	// concept storage and only the OS env fallback keeps them alive.
	// See docs/public/operate/env-vars.md for the full bootstrap-order story.

	// Cross-registry CQS pass (Phase 2 hoist). Catches the
	// cross-file violations the per-file ValidateCQS in
	// tryParseNewFunctionSyntax misses: a query in file A calling
	// a mutation in file B is rejected here, fail-loud at engine
	// startup rather than silently corrupting data when the call
	// site fires for the first time. No escape valve -- the project
	// starts fresh under the new rule.
	if err := ValidateCQSAcrossRegistry(functionRegistry); err != nil {
		return err
	}

	// DSL dependency-tree validation (grammar redesign C3/#2043).
	// Replaces the retired naming-prefix convention: every structural
	// reference (a query's `shape` slot, a shape's `include`) must
	// resolve to a construct of the enclosing concept, or be an
	// explicit `concept.name` cross-concept reference. A violation is a
	// hard load-time error with an actionable fix message. No escape
	// valve -- the project starts fresh under the new rule.
	if err := ValidateDependencyTree(); err != nil {
		return err
	}

	// Story 6 (#2304 / ADR §2.2): in-memory-vs-SQL guardrail lint. Warn
	// (never fail) when a logic body runs a Story 4 collection chain over an
	// unfiltered full-concept query read -- that work belongs in a query
	// `filter` / SQL pushdown, not an in-memory scan. Mirrors the dead-logic
	// lint's warning severity; load is unaffected.
	warnInMemoryCollectionScans(e.Logger, functionRegistry.Snapshot())

	// Capability catalog + authored-action validation (construct-invocation
	// epic #2322, Story 8 / memql#2330). The DSL `capability` declarations are
	// reconciled against the Go vocabulary registry (every declared
	// capability's @sideEffect must equal the authoritative
	// capability.CapabilityClass, and its dotted name must be in the closed
	// surface-backed set), and every authored `action` is loaded under STRICT
	// arg-typing (each capability call's args validated against the declared
	// capability schema). A reconciliation or strict-typing failure crashes the
	// cluster at bootstrap, so surface it here as a hard load-time error.
	if err := actions.DefaultCatalogError(); err != nil {
		return fmt.Errorf("capability catalog reconciliation failed: %w", err)
	}
	if err := actions.DefaultLoadError(); err != nil {
		return fmt.Errorf("authored action load (strict capability arg-typing) failed: %w", err)
	}

	// Load-time per-kind uniqueness gate (epic #2351 / S5, memql#2360),
	// folded into the S2 LoadReport. Two authored constructs that land in
	// the SAME runtime registry -- query / mutation / logic / builtin ->
	// FunctionRegistry, spec / trait -> SpecRegistry, shapes / tools /
	// prompts / providers / policies / seeds / automations / actions ->
	// their own -- with the same bare name silently overwrite one another
	// by load order (Upsert never checks; Add drops the loser). This walks
	// the same merged tree the loaders register from (embedded core + every
	// RegisterTree'd pack, each file once), so on a carrier node the check
	// spans core + the copresent pack. S5 gave this ERROR visibility; S2
	// makes it a strict-boot signal (below) alongside the skipped
	// constructs each loader recorded on the report.
	dups := DetectDuplicateConstructs(baseloader.ReadAll(e.Logger))
	report.SetDuplicates(dups)
	if e.Logger != nil && len(dups) > 0 {
		for _, d := range dups {
			e.Logger.Error("duplicate DSL construct registration (silent last-wins)",
				"component", "memql.engine",
				"group", d.Group,
				"name", d.Name,
				"origins", strings.Join(d.Origins, " | "),
				"count", len(d.Origins))
		}
		e.Logger.Error("DSL uniqueness gate: duplicate construct name(s) detected across core + packs",
			"component", "memql.engine",
			"duplicateCount", len(dups))
	}

	// Strict DSL boot (epic #2351 / S2, memql#2357). A skipped in-tree
	// construct (any unified loader's parse/register drop) or a same-
	// registry duplicate means the embedded core tree or a registered pack
	// HALF-loaded. A half-loaded pack is worse than one that fails: a
	// multi-node fleet boots green with arbitrary construct subsets (the
	// copresent-rot mechanism, bff#153). Refuse to boot so the failure is
	// loud + uniform instead of silent + per-node. MEMQL_DSL_ALLOW_SKIPS is
	// the operator break-glass. NB: durably-promoted authored bundles are
	// NOT gated here -- their stored source can rot when the grammar moves
	// and must not brick a fleet, so the boot re-hydration path quarantines
	// them (ERROR + report entry) rather than failing boot.
	if report.HasProblems() {
		if !dslAllowSkips() {
			return fmt.Errorf("strict DSL boot refused: %d problem(s) loading the embedded tree + registered packs (%d skipped construct(s), %d duplicate(s)); set %s=1 to boot anyway (operator break-glass).\n%s",
				report.ProblemCount(), len(report.Skipped), len(report.Duplicates), allowSkipsEnvVar, report.Detail())
		}
		if e.Logger != nil {
			e.Logger.Error("MEMQL_DSL_ALLOW_SKIPS set: booting despite DSL load problems (operator break-glass)",
				"component", "memql.engine",
				"problems", report.ProblemCount(),
				"skipped", len(report.Skipped),
				"duplicates", len(report.Duplicates),
				"detail", report.Detail())
		}
	}

	// Log boot validation summary + the structured load-report summary
	// (one healthy line, or ERROR + per-problem detail on skips/dups).
	e.logBootValidationSummary(functionRegistry, shapeRegistry, specRegistry, providerRegistry)
	report.logSummary(e.Logger)

	e.initialized = true

	return nil
}

// ReloadAIProviders re-loads the AI provider registry from .memql
// files and re-resolves auth placeholders. Intended for the dev-
// refresh workflow: after wiping the database and re-seeding
// secrets, providers that eager-loaded against an empty concept
// storage need to reload so their auth picks up the freshly seeded
// values from v1:platform:globalSecret instead of falling back to OS env.
//
// Concurrency: assumes the engine is in a quiescent state (no
// in-flight AI calls). The dev-refresh workflow runs this after
// `make secrets-seed` completes and before any user traffic arrives.
// Production callers (post-rotation) should similarly drain in-flight
// calls before invoking; the swap is non-atomic across providers.
//
// Returns the count of providers loaded + any non-fatal load errors
// (provider files that failed auth resolution are skipped, not
// fatal). nil error means nothing failed.
