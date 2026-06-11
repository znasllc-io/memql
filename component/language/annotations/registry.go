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
		"description", "enabled", "disabled", "internal", "public",
	},
	"Mutation": {
		"description", "enabled", "disabled", "internal", "actor", "public",
		"mergeFields",
	},
	"Logic": {
		"description", "enabled", "disabled",
	},
	"Automation": {
		"description", "enabled", "disabled", "trigger", "filter",
	},
	"Spec": {
		"description", "enabled", "disabled", "shape",
	},
	"Tool": {
		"description", "enabled", "disabled", "handler", "executionTime",
		"destructive", "requiresConfirmation", "rateLimit",
		"clientExecution", "allowedRoles", "scopes",
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
	"": { // top-level (concept definitions)
		"description", "version", "namespace", "scope", "visibility", "type", "cache", "relationship",
	},
}

// Docs maps an annotation name to its one-line hover/completion doc.
// Every name offered by ByReceiver carries an entry here (enforced by
// TestEveryAnnotationHasDoc in component/memql/sense).
var Docs = map[string]string{
	// Lifecycle / shared.
	"enabled":     "Enable this definition. Functions are disabled by default.",
	"disabled":    "Disable this definition.",
	"description": "Human-readable description of this definition.",
	"internal":    "Hide from external API discovery.",
	"public":      "Per-row-authz marker: this query/mutation is intentionally callable without a caller-scope filter (concept catalogs, pre-auth login paths). See docs/public/operate/auth/per-row-authz-audit.md.",
	"actor":       "On a mutation: resolves auth-context (`actor.X`) fields. On a shape: kind marker -- projects the auth-context envelope (actor.userId / role / ...).",
	"mergeFields": "On an update mutation: deep-merge the named object-typed payload fields into the stored object instead of replacing them wholesale, so sibling keys survive a single-key write. Format: @mergeFields(\"preferences\").",
	// Query / spec.
	"shape": "Optional: pin the shape a spec's predicate reads (the eval strategy is otherwise derived from the body's field references).",
	// Automation.
	"trigger": "Event trigger for automations. Format: @trigger(event=\"graph.node.created.*.v1:ns:concept\") or @trigger(schedule=\"0 0 * * * *\").",
	"filter":  "Filter expression for automation triggers.",
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
	"defaultProvider": "Default SI provider for prompt execution.",
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
	// Concept.
	"version":      "Version tag for a concept.",
	"namespace":    "Concept namespace. Colon-separated lowercase identifiers (e.g., \"cognition\" or \"cognition:client:tool\").",
	"scope":        "Partition scope. @scope(\"global\") places rows in the reserved _system partition; default is partition-scoped.",
	"visibility":   "Which node types load this concept. @visibility(\"*\"), @visibility(\"cognition\", \"bff\"), or @visibility(!\"planner\").",
	"cache":        "Concept query result caching. Format: @cache(ttl=\"5m\").",
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
