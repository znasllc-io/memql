package memql

import (
	"context"
	"fmt"
	"strings"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (e *MemQLEngine) Init(concepts concept.Registry) error {
	e.relationships = relationshipRegistry{}
	e.concepts = nil
	e.initialized = false
	e.prompts = nil
	e.providers = nil
	e.functions = nil
	e.tools = nil
	e.siRuntime = nil

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
	if _, _, ulErr := LoadUnifiedFunctions(e.Logger, functionRegistry, e.concepts); ulErr != nil {
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
	if _, ulErr := LoadUnifiedShapes(e.Logger, shapeRegistry); ulErr != nil {
		e.Logger.Warn("unified shape loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}

	// Load specs + traits from the new tree via the dedicated parser.
	// Specs / traits use the SpecRegistry; the unified function
	// loader does NOT handle them. Without this call the registry
	// stays empty + every executor lookup misses.
	if _, ulErr := LoadUnifiedSpecs(e.Logger, specRegistry); ulErr != nil {
		e.Logger.Warn("unified spec loader returned an error",
			"component", "memql.engine", "error", ulErr)
	}

	// Now that both spec and shape registries are populated, validate
	// every spec's @useShape(N) binding resolves to a registered
	// shape. Catches typos + dangling references at startup.
	if err := ValidateSpecBindings(specRegistry, shapeRegistry); err != nil {
		return err
	}

	// Pass 2: overlay builtins from the new tree (struct-form
	// `builtin NAME { ... }` blocks). Builtins are functions
	// internally so they merge into the FunctionRegistry.
	if _, ulErr := LoadUnifiedBuiltins(e.Logger, functionRegistry); ulErr != nil {
		e.Logger.Warn("unified builtin loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}

	// Load MCP tools
	toolRegistry, err := loadToolRegistry(e.Logger)
	if err != nil {
		return err
	}

	// Pass 2: overlay tools from the new tree.
	if _, ulErr := LoadUnifiedTools(e.Logger, toolRegistry); ulErr != nil {
		e.Logger.Warn("unified tool loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}

	// Auto-register enabled MemQL functions as MCP tools (Option A).
	// This exposes functions to tool-calling agents without allowing free-form MemQL.
	registerFunctionTools(e.Logger, functionRegistry, toolRegistry)

	e.tools = toolRegistry

	promptRegistry, err := loadPromptRegistry(e.Logger)
	if err != nil {
		return err
	}

	// Pass 2: overlay prompts from the new tree. The unified loader
	// resolves @templateFile references against dsl.Tree() so the
	// .tmpl sidecars in dsl/<domain>/prompts/ are picked up.
	// Reuse the same partials the legacy loader built.
	if partials, pErr := loadPartials(); pErr == nil && partials != nil {
		if _, ulErr := LoadUnifiedPrompts(e.Logger, promptRegistry, partials); ulErr != nil {
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
	if _, slErr := LoadUnifiedSeeds(e.Logger, seedRegistry); slErr != nil {
		e.Logger.Warn("unified seed loader returned an error",
			"component", "memql.engine", "error", slErr)
	}
	e.seeds = seedRegistry
	e.seedMaterializer = NewSeedMaterializer(e, e.seeds)

	// Wire concept-storage resolvers BEFORE loading providers so a
	// provider's auth.apiKey="${MEMQL_SI_OPENAI_API_KEY}" picks up the
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

	providerRegistry, err := loadSIProviders(e.Logger)
	if err != nil {
		return err
	}

	// Pass 2: overlay providers from the new tree. dsl/providers/
	// has one file per vendor with the base + all models in one
	// place; each `provider NAME { ... }` block becomes a registry
	// entry.
	if _, ulErr := LoadUnifiedProviders(e.Logger, providerRegistry); ulErr != nil {
		e.Logger.Warn("unified provider loader returned an error; legacy covers gap",
			"component", "memql.engine", "error", ulErr)
	}
	policyRegistry, err := loadSIPolicies(e.Logger)
	if err != nil {
		return err
	}
	if _, ulErr := LoadUnifiedPolicies(e.Logger, policyRegistry); ulErr != nil {
		e.Logger.Warn("unified policy loader returned an error; legacy stub covers gap",
			"component", "memql.engine", "error", ulErr)
	}
	policyFunctionRegistry, err := loadPolicyFunctions(e.Logger)
	if err != nil {
		return err
	}
	if _, ulErr := LoadUnifiedPolicyFunctions(e.Logger, policyFunctionRegistry); ulErr != nil {
		e.Logger.Warn("unified policy-function loader returned an error; legacy stub covers gap",
			"component", "memql.engine", "error", ulErr)
	}
	e.prompts = promptRegistry
	e.providers = providerRegistry
	e.policies = policyRegistry
	e.policyFunctions = policyFunctionRegistry
	e.siRuntime = newSIRuntime(e.Logger, promptRegistry, providerRegistry, e.siCacheConfig)

	// ReloadSIProviders is exposed below so dev workflows that wipe
	// the database before re-seeding (make dev-refresh) can refresh
	// provider auth resolution AFTER seeding completes -- without
	// this hook, providers eager-load before secrets exist in
	// concept storage and only the OS env fallback keeps them alive.
	// See docs/guides/env-vars.md for the full bootstrap-order story.
	
	// Log boot validation summary
	e.logBootValidationSummary(functionRegistry, shapeRegistry, specRegistry, providerRegistry)
	
	e.initialized = true

	return nil
}

// ReloadSIProviders re-loads the SI provider registry from .memql
// files and re-resolves auth placeholders. Intended for the dev-
// refresh workflow: after wiping the database and re-seeding
// secrets, providers that eager-loaded against an empty concept
// storage need to reload so their auth picks up the freshly seeded
// values from v1:platform:globalSecret instead of falling back to OS env.
//
// Concurrency: assumes the engine is in a quiescent state (no
// in-flight SI calls). The dev-refresh workflow runs this after
// `make secrets-seed` completes and before any user traffic arrives.
// Production callers (post-rotation) should similarly drain in-flight
// calls before invoking; the swap is non-atomic across providers.
//
// Returns the count of providers loaded + any non-fatal load errors
// (provider files that failed auth resolution are skipped, not
// fatal). nil error means nothing failed.
