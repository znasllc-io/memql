// Package annotations is the single physical source of truth for the
// memQL construct-annotation surface: which annotations each receiver
// kind accepts (ByReceiver) and the one-line doc for each annotation
// (Docs).
//
// It is a leaf package — it imports nothing inside the repo — so both
// the engine-side load gate (component/memql) and the editor/sense
// surface (component/memql/sense) can derive from it without an import
// cycle. Before this package existed the two surfaces each kept their
// own hand-maintained copy and a consistency test guarded them against
// drift (#991); now there is one copy and the test guards that the
// derived views still agree.
//
// Scope: this registry backs the four function constructs' load-time
// allow-list (Query / Mutation / Logic / Automation) and the editor's
// completion/diagnose/hover surface for every receiver. The per-
// construct decl parsers in component/language/parser (tool_decl.go,
// provider_decl.go, ...) remain the authoritative parse-time gate for
// the declarative constructs — their accepted sets are mirrored here
// for the editor, kept in sync by review.
package annotations

// ByReceiver maps a receiver kind to the annotation names it accepts.
// The empty-string key is the top-level concept-definition receiver.
//
// For the four function constructs (Query / Mutation / Logic /
// Automation) this is the canonical allow-list the load-time validator
// consults (via component/memql). For the declarative constructs it is
// the editor projection of each decl parser's accepted set.
var ByReceiver = map[string][]string{
	"Query": {
		"description", "enabled", "disabled", "internal", "public", "mcp", "unbounded", "cache", "nocache", "latestMode",
	},
	"Mutation": {
		"description", "enabled", "disabled", "internal", "actor", "public",
		"mergeFields", "appendFields", "createOnly", "scrubPii", "mcp",
	},
	"Logic": {
		"description", "enabled", "disabled", "eventField",
	},
	"Automation": {
		"description", "enabled", "disabled", "trigger", "filter", "mcp",
	},
	"Action": {
		"description", "enabled", "disabled", "kind", "sideEffect",
	},
	"Capability": {
		"description", "enabled", "disabled", "sideEffect",
	},
	"Spec": {
		"description", "enabled", "disabled",
	},
	"Tool": {
		"description", "enabled", "disabled", "handler", "executionTime",
		"destructive", "requiresConfirmation", "rateLimit",
		"clientExecution", "allowedRoles", "scopes", "mcp",
	},
	"Builtin": {
		"description", "enabled", "disabled", "internal", "executor", "alias", "args", "sdk",
	},
	"Prompt": {
		"description", "enabled", "disabled", "defaultProvider", "templateFile",
	},
	"Provider": {
		"description", "type", "model", "modality", "default", "base", "extends",
	},
	"Shape": {
		"description", "row", "actor",
	},
	"Policy": {
		"description", "primary", "fallback", "maxLatencyMs", "maxTimeToFirstTokenMs", "preferredRole",
	},
	"Seed": {
		"description", "version", "namespace", "scope", "templateFile", "enabled", "disabled",
	},
	"": { // top-level (concept definitions)
		"description", "version", "namespace", "scope", "visibility", "type", "cache", "relationship",
	},
}

// Docs maps an annotation name to its one-line hover/completion doc.
// Every name offered by ByReceiver carries an entry here (enforced by
// TestEveryAnnotationHasDoc in component/memql/sense).
var Docs = map[string]string{
	// Lifecycle / shared.
	"enabled":      "Enable this definition. Functions are disabled by default.",
	"disabled":     "Disable this definition.",
	"description":  "Human-readable description of this definition.",
	"eventField":   "On an event-triggered logic: declare the allowed top-level event payload fields (e.g. @eventField(\"partitionId\", \"siParticipantId\")). Opt-in field-level validation -- every event.payload.<field> reference in the body is checked against this set at load time, rejecting typos / fields the (possibly synthetic, handler-assembled) triggering event cannot carry (memql#1743). Bare names or payload.-prefixed paths both normalize to the head segment.",
	"internal":     "Hide from external API discovery.",
	"public":       "Per-row-authz marker: this query/mutation is intentionally callable without a caller-scope filter (concept catalogs, pre-auth login paths). See docs/public/operate/auth/per-row-authz-audit.md.",
	"actor":        "On a mutation: resolves auth-context (`actor.X`) fields. On a shape: kind marker -- projects the auth-context envelope (actor.userId / role / ...).",
	"mergeFields":  "On an update mutation: deep-merge the named object-typed payload fields into the stored object instead of replacing them wholesale, so sibling keys survive a single-key write. Format: @mergeFields(\"preferences\").",
	"appendFields": "On an update mutation: append the named array-typed payload fields' elements to the stored array instead of replacing it wholesale, so a single-writer mutation can accumulate list items (e.g. attach one id). Format: @appendFields(\"attachmentIds\").",
	"createOnly":   "On an insert (create-or-upsert) mutation: write the named payload fields ONLY when creating the row. If the target id already exists, the fields are dropped from the delta before the engine read-merge, so the stored value is preserved rather than clobbered -- making a deterministic-id re-stage idempotent for lifecycle fields another writer owns after creation (e.g. stageOutboundRequest seeds status but must not reset a row the outbound worker moved to sent). The inverse of @mergeFields/@appendFields: only valid on insert-kind mutations. Format: @createOnly(\"status\", \"attempts\"). See fylo#63.",
	"scrubPii":     "On an update mutation (the hard-delete / data-deletion path): after the partial payload merges, zero EVERY field the bound concept marks @pii. The field set is derived from the schema, so a newly-annotated PII field is scrubbed automatically with no change to the mutation. Bare flag, no arguments. See memql#1711.",
	// Automation.
	"trigger": "Event trigger for automations. Format: @trigger(event=\"graph.node.created.*.v1:ns:concept\") or @trigger(schedule=\"0 0 * * * *\").",
	"filter":  "Filter expression for automation triggers.",
	// Action (memql#2218, behavioral-constructs ADR §2.3).
	"kind":       "On an action: the action kind. @kind(\"primitive\") = one external capability rendered from params (the only kind today; composites collapse into automations, ADR §2.2).",
	"sideEffect": "On an action: coarse risk class @sideEffect(\"read\"|\"write\"|\"exec\") carried for authoring + the surface-aware trust gate. The AUTHORITATIVE sideEffectClass lives on the capability (ADR §7) so an authored/generated action cannot spoof it.",
	// Pagination opt-out (epic 5, memql#1965).
	"unbounded": "On a list-returning query: opt out of the pagination authoring rule and the implicit 50-row runtime cap. Format: @unbounded(\"reason\"). The reason string is REQUIRED -- it documents why this query is a legitimate full-set read (small bounded catalog, sweep job, etc.) and is enumerated by the pagination audit report. A query that paginates/sorts is already bounded and must NOT carry @unbounded; the engine clamps the realized window to MEMORY_ENGINE_MAX_WINDOW regardless. See docs/public/language/authoring-rules.md.",
	// Temporal-access visibility (core-builtins ADR §2.3, memql#2305).
	"latestMode": "On a query: marks the query as time-dependent because it reads `asOf latest` (the live tip of the append-only stream), so its result is clock-dependent / not reproducible. The engine AUTO-DERIVES this from a `asOf latest` clause in the body, so the annotation is an explicit, reader-facing restatement of that contract -- not a switch. A query with `asOf <explicit timestamp>` is deterministic and is NOT time-dependent. See core-builtins-and-collections-adr.md §2.3.",
	// MCP promotion (epic memql#1529 Phase 4 #1534).
	"mcp": "Expose this construct on the MCP connector surface. On a query/mutation/automation it promotes the construct into its own first-class MCP tool (otherwise it stays reachable via the generic run_query / run_mutation / run_automation dispatchers). On a tool it opts the tool into the curated connector allowlist: once ANY tool carries @mcp, tools/list reflects only @mcp tools (otherwise -- zero tagged -- the full tool surface is reflected, so the annotation is inert until the curated set is tagged).",
	// Tool.
	"handler":              "Tool handler configuration. Format: @handler(type=\"query\", query=\"...\") / @handler(type=\"function\", name=\"...\").",
	"executionTime":        "Expected execution time hint: \"fast\", \"medium\", or \"slow\".",
	"destructive":          "Mark a tool as destructive (mutates/deletes); the tool loop gates it behind a confirmation.",
	"requiresConfirmation": "Require explicit user confirmation before the tool executes.",
	"rateLimit":            "Tool rate limiting. Format: @rateLimit(maxCalls=100, periodSeconds=3600).",
	"clientExecution":      "Mark a tool as client-executed (runs in the browser, relayed agent->browser).",
	"allowedRoles":         "Restrict the tool to a set of agent roles.",
	"scopes":               "Authorization scopes the tool requires.",
	// Builtin.
	"executor": "Go executor name for builtin functions (integration.X.Y).",
	"args":     "Parse-time argument contract for builtin functions.",
	"alias":    "Additional name the builtin is registered under.",
	"sdk":      "Generator marker (sdk/gen reads from source); no engine effect.",
	// Prompt.
	"defaultProvider": "Default AI provider for prompt execution.",
	"templateFile":    "External template file path for prompts.",
	// Provider.
	"type":     "Provider vendor type (e.g., \"OpenAI\", \"Anthropic\"); on a concept, the row kind (\"object\"/\"collection\"/\"reference\").",
	"model":    "Model identifier (e.g., \"gpt-5.4-mini\", \"claude-sonnet-4-6\").",
	"modality": "Provider modality (e.g., \"chat\", \"audio\", \"image\", \"embedding\").",
	"default":  "Mark this provider as the default for its modality; on a field, the value used when the caller omits it.",
	"base":     "Mark a vendor-level base provider (auth + type only).",
	"extends":  "Inherit configuration from a base provider.",
	// Shape.
	"row": "Shape kind: projects a concept's payload + row intrinsics (concept bound via the `shape <Concept> <name>` signature).",
	// Policy (AI Router provider-selection records). @primary is required; @fallback / @preferredRole are repeatable.
	"primary":               "Required on a policy: the primary provider name the AI Router resolves first.",
	"fallback":              "On a policy: a fallback provider tried when the primary is unavailable. Repeatable (order preserved).",
	"maxLatencyMs":          "On a policy: cap the acceptable provider latency in milliseconds for selection.",
	"maxTimeToFirstTokenMs": "On a policy: cap the acceptable time-to-first-token (streaming) in milliseconds for selection.",
	"preferredRole":         "On a policy: bias selection toward providers matching this agent role. Repeatable.",
	// Concept.
	"version":      "Version tag for a concept.",
	"namespace":    "Concept namespace. Colon-separated lowercase identifiers (e.g., \"cognition\" or \"cognition:client:tool\").",
	"scope":        "Partition scope. @scope(\"global\") places rows in the reserved _system partition; default is partition-scoped.",
	"visibility":   "Which node types load this concept. @visibility(\"*\"), @visibility(\"cognition\", \"bff\"), or @visibility(!\"planner\").",
	"cache":        "Override the result-cache TTL for the query. Format: @cache(ttl=\"300\") -- a whole number of SECONDS (string-valued). Pure reads cache BY DEFAULT (60s backstop) without this annotation (5.6); @cache sets a different TTL, longer or shorter. @cache(ttl=\"0\") is the explicit \"never cache\" opt-out (or use @nocache). The engine keys the cache on the plan signature (query/sort/limit/depth/shape + the keyset cursor) and evicts on any write to the read concept via the cache.invalidate.* broadcast channel (5.4/5.6 invalidation) -- cross-node eviction needs no per-concept routing rule. See docs/internal/planning/cache-audit-phase-0.md.",
	"nocache":      "Opt this query OUT of caching entirely (force \"never cache\"). Clearer alias for @cache(ttl=\"0\"); use it for reads that must always be live (auth, monotonic counters, presence). Pure reads cache by default (5.6), so @nocache is the escape for the rare read where even brief staleness is wrong.",
	"relationship": "Foreign-key relationship metadata. Format: @relationship(type=\"parent\", field=\"x\", target=\"v1:ns:concept\", direction=\"outgoing\").",
}

// Set returns the receiver's accepted annotation names as a membership
// set. Unknown receivers yield an empty (non-nil) set. The result is a
// fresh map the caller may keep; mutating it does not affect the
// registry.
func Set(receiver string) map[string]bool {
	names := ByReceiver[receiver]
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}
