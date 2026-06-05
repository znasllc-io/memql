package sense

// BuiltinDef describes a built-in function's signature for completion and hover.
type BuiltinDef struct {
	Signature  string
	Doc        string
	Parameters []Parameter
}

// ReceiverTypes lists all valid receiver types for func declarations.
var ReceiverTypes = []string{
	"Query", "Mutation", "Automation", "Spec", "Tool", "Builtin", "Prompt", "Provider", "Shape",
}

// Keywords lists all MemQL keywords.
var Keywords = []string{
	"func", "for", "range", "if", "else", "switch", "case", "default",
	"continue", "break", "return", "nil", "retry", "when", "as", "where",
	"use", "concept", "in", "has", "not",
}

// BuiltinFunctions maps builtin function names to their signatures and documentation.
var BuiltinFunctions = map[string]BuiltinDef{
	"si": {
		Signature: `si(templateId string, data object, provider? string)`,
		Doc:       "Invoke a synthetic intelligence prompt template with the given data.",
		Parameters: []Parameter{
			{Label: "templateId", Documentation: "Name of the prompt template to invoke."},
			{Label: "data", Documentation: "Data object passed to the template."},
			{Label: "provider", Documentation: "Optional provider override (e.g., \"claudeSonnet\")."},
		},
	},
	"node": {
		Signature:  `node(field string)`,
		Doc:        "Access a field from the current node in a shape template.",
		Parameters: []Parameter{{Label: "field", Documentation: "Dot-separated field path (e.g., \"payload.name\")."}},
	},
	"children": {
		Signature:  `children(concept string)`,
		Doc:        "Retrieve child nodes of the current node for a given concept.",
		Parameters: []Parameter{{Label: "concept", Documentation: "Child concept name (e.g., \"v1:cognition:participant\")."}},
	},
	"parent": {
		Signature:  `parent(concept string)`,
		Doc:        "Retrieve the parent node for a given concept.",
		Parameters: []Parameter{{Label: "concept", Documentation: "Parent concept name."}},
	},
	"payload": {
		Signature:  `payload(field string)`,
		Doc:        "Access a field from the event payload.",
		Parameters: []Parameter{{Label: "field", Documentation: "Dot-separated field path."}},
	},
	"similar": {
		Signature: `similar(text string, concept string, field string, topK? int, minScore? float)`,
		Doc:       "Find semantically similar nodes using vector search (pgvector).",
		Parameters: []Parameter{
			{Label: "text", Documentation: "Query text to find similar content for."},
			{Label: "concept", Documentation: "Concept to search within."},
			{Label: "field", Documentation: "Vector field name (e.g., \"content\")."},
			{Label: "topK", Documentation: "Maximum number of results (default: 10)."},
			{Label: "minScore", Documentation: "Minimum similarity score 0-1 (default: 0.5)."},
		},
	},
	"embed": {
		Signature: `embed(text string, model? string)`,
		Doc:       "Generate an embedding vector for text using the configured provider.",
		Parameters: []Parameter{
			{Label: "text", Documentation: "Text to embed."},
			{Label: "model", Documentation: "Optional model override (default: text-embedding-3-small)."},
		},
	},
	"concat": {
		Signature: `concat(values ...string)`,
		Doc:       "Concatenate multiple string values.",
		Parameters: []Parameter{
			{Label: "values", Documentation: "Strings to concatenate."},
		},
	},
	"coalesce": {
		Signature: `coalesce(values ...any)`,
		Doc:       "Return the first non-nil value.",
		Parameters: []Parameter{
			{Label: "values", Documentation: "Values to check."},
		},
	},
	"cond": {
		Signature: `cond(predicate, thenValue, elseValue)`,
		Doc:       "Return thenValue when predicate is truthy, else elseValue. The canonical conditional-value expression; use the `if` statement for control flow.",
		Parameters: []Parameter{
			{Label: "predicate", Documentation: "Boolean-valued expression."},
			{Label: "thenValue", Documentation: "Value returned when predicate is truthy."},
			{Label: "elseValue", Documentation: "Value returned when predicate is falsy."},
		},
	},
	"lower": {
		Signature:  `lower(value string)`,
		Doc:        "Convert string to lowercase.",
		Parameters: []Parameter{{Label: "value", Documentation: "String to convert."}},
	},
	"upper": {
		Signature:  `upper(value string)`,
		Doc:        "Convert string to uppercase.",
		Parameters: []Parameter{{Label: "value", Documentation: "String to convert."}},
	},
	"trim": {
		Signature:  `trim(value string)`,
		Doc:        "Remove leading and trailing whitespace.",
		Parameters: []Parameter{{Label: "value", Documentation: "String to trim."}},
	},
	"contains": {
		Signature: `contains(haystack string, needle string)`,
		Doc:       "Check if a string contains a substring.",
		Parameters: []Parameter{
			{Label: "haystack", Documentation: "String to search in."},
			{Label: "needle", Documentation: "Substring to find."},
		},
	},
	"hash": {
		Signature:  `hash(value string)`,
		Doc:        "Compute SHA-256 hash of a string value.",
		Parameters: []Parameter{{Label: "value", Documentation: "String to hash."}},
	},
	"canonicalId": {
		Signature: `canonicalId(value, concept)`,
		Doc: "Normalize an id-shaped value to canonical form (`<partition>:<concept>:<bareSlug>`).\n\n" +
			"Use in mutation id derivations that hash foreign-key args, so the derived id stays stable whether the caller passes a bare slug or an already-canonical id.\n\n" +
			"Example: `id = concat(\"participant-\", hash(concat(canonicalId(args.spaceId, space), \":\", canonicalId(args.userId, user))))`\n\n" +
			"The second argument is an imported concept short-name (resolved against the file-top `use ...concepts.{ ... }` imports); the stringly-typed `\"v1:ns:name\"` literal is retired. The engine reads the named concept's @scope to pick the right partition prefix (`_system` for global, otherwise the request envelope's partition). Errors when the concept name isn't imported / registered or when the value is already canonical for a different concept (catches type-tag typos).",
		Parameters: []Parameter{
			{Label: "value", Documentation: "Id-shaped value (bare slug or canonical). Empty input returns empty."},
			{Label: "concept", Documentation: "Imported concept short-name, e.g. `user` (from `use identity.concepts.{ user }`)."},
		},
	},
	"first": {
		Signature:  `first(collection)`,
		Doc:        "Return the first item from a collection.",
		Parameters: []Parameter{{Label: "collection", Documentation: "Collection to get first item from."}},
	},
	"last": {
		Signature:  `last(collection)`,
		Doc:        "Return the last item from a collection.",
		Parameters: []Parameter{{Label: "collection", Documentation: "Collection to get last item from."}},
	},
	"addDuration": {
		Signature: `addDuration(timestamp, duration string)`,
		Doc:       "Add a duration to a timestamp (e.g., \"24h\", \"30m\").",
		Parameters: []Parameter{
			{Label: "timestamp", Documentation: "Base timestamp."},
			{Label: "duration", Documentation: "Duration string (e.g., \"24h\", \"7d\")."},
		},
	},
	"daysBetween": {
		Signature: `daysBetween(start, end)`,
		Doc:       "Calculate the number of days between two timestamps.",
		Parameters: []Parameter{
			{Label: "start", Documentation: "Start timestamp."},
			{Label: "end", Documentation: "End timestamp."},
		},
	},
	"now": {
		Signature:  `now()`,
		Doc:        "Return the current timestamp.",
		Parameters: nil,
	},
	"timestamp": {
		Signature:  `timestamp(value? string)`,
		Doc:        "Parse or return a timestamp. With no arguments, same as now().",
		Parameters: []Parameter{{Label: "value", Documentation: "Optional timestamp string to parse."}},
	},
	"error": {
		Signature:  `error(message string)`,
		Doc:        "Return an error, terminating the current execution.",
		Parameters: []Parameter{{Label: "message", Documentation: "Error message."}},
	},
	"toString": {
		Signature:  `toString(value any)`,
		Doc:        "Convert a value to its string representation.",
		Parameters: []Parameter{{Label: "value", Documentation: "Value to convert."}},
	},
	"var": {
		Signature:  `var(name string)`,
		Doc:        "Resolve a partition-scoped plaintext configuration variable from v1:platform:partitionVariable. Falls back to v1:platform:globalVariable (global) if the partition lookup misses.",
		Parameters: []Parameter{{Label: "name", Documentation: "Variable name (e.g., \"MEMQL_COGNITION_SI_ENABLED\")."}},
	},
	"systemVar": {
		Signature:  `systemVar(name string)`,
		Doc:        "Resolve an instance-wide (global) plaintext configuration variable from v1:platform:globalVariable. No fallback.",
		Parameters: []Parameter{{Label: "name", Documentation: "System variable name (e.g., \"MEMQL_DEFAULT_CHAT_PROVIDER\")."}},
	},
	"secret": {
		Signature:  `secret(name string)`,
		Doc:        "Resolve a partition-scoped encrypted secret from v1:platform:partitionSecret. Falls back to v1:platform:globalSecret (global) if the partition lookup misses. Returns the decrypted plaintext; requires MEMQL_MASTER_KEY. Callers must never log the result.",
		Parameters: []Parameter{{Label: "name", Documentation: "Secret name (e.g., \"OPENAI_API_KEY\")."}},
	},
	"systemSecret": {
		Signature:  `systemSecret(name string)`,
		Doc:        "Resolve an instance-wide (global) encrypted secret from v1:platform:globalSecret. No fallback. Returns the decrypted plaintext; requires MEMQL_MASTER_KEY.",
		Parameters: []Parameter{{Label: "name", Documentation: "System secret name (e.g., \"IDENTITY_KEY_ENCRYPTION_KEY\")."}},
	},
	"step": {
		Signature:  `step(name string)`,
		Doc:        "Reference the result of a previous automation step.",
		Parameters: []Parameter{{Label: "name", Documentation: "Step identifier."}},
	},
	"input": {
		Signature:  `input()`,
		Doc:        "Access the automation's input data.",
		Parameters: nil,
	},
	"item": {
		Signature:  `item()`,
		Doc:        "Access the current item in a forEach loop.",
		Parameters: nil,
	},
	"index": {
		Signature:  `index()`,
		Doc:        "Access the current index in a forEach loop.",
		Parameters: nil,
	},
	"event": {
		Signature:  `event()`,
		Doc:        "Access the trigger event data in an automation.",
		Parameters: nil,
	},
}

// AnnotationsByReceiver maps receiver types to their valid annotations. This is
// the editor projection of the per-construct annotation surface; for the four
// function constructs (Query / Mutation / Logic / Automation) it is held
// identical to the parser gate (`constructAnnotationAllowLists`) by
// TestAnnotationReceiverGateConsistency (#991), which fails if they drift. The
// other constructs mirror their per-construct parser/converter accepted sets.
var AnnotationsByReceiver = map[string][]string{
	"Query": {
		"description", "enabled", "disabled", "internal", "public",
	},
	"Mutation": {
		"description", "enabled", "disabled", "internal", "actor", "public",
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

// AnnotationDocs maps annotation names to documentation strings. Note that a
// few names are deliberately per-construct overloaded (resolved by which
// construct they sit on): `@type` is a provider vendor type AND a concept row
// kind ("object" / "collection" / "reference"); `@default` is a provider
// default-flag AND an args/concept field default. The doc below states the
// common meaning.
var AnnotationDocs = map[string]string{
	// Lifecycle / shared.
	"enabled":     "Enable this definition. Functions are disabled by default.",
	"disabled":    "Disable this definition.",
	"description": "Human-readable description of this definition.",
	"internal":    "Hide from external API discovery.",
	"public":      "Per-row-authz marker: this query/mutation is intentionally callable without a caller-scope filter (concept catalogs, pre-auth login paths). See docs/auth/per-row-authz-audit.md.",
	"actor":       "On a mutation: resolves auth-context (`actor.X`) fields. On a shape: kind marker -- projects the auth-context envelope (actor.userId / role / ...).",
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

// KeywordDocs maps keywords to documentation strings.
var KeywordDocs = map[string]string{
	"func":     "Declare a function with a receiver type: func (ReceiverType) name(args) { ... }",
	"for":      "Loop over a range: for item := range collection { ... }",
	"range":    "Used with for to iterate: for item := range collection",
	"if":       "Conditional execution: if condition { ... } else { ... }",
	"else":     "Alternative branch in an if statement.",
	"switch":   "Multi-way branch: switch value { case X: ... default: ... }",
	"case":     "A branch in a switch statement.",
	"default":  "Default branch in a switch statement.",
	"continue": "Skip to the next iteration of a for loop.",
	"break":    "Exit the current for loop.",
	"return":   "Return a value from a function.",
	"nil":      "The nil value (absence of a value).",
	"retry":    "Retry an expression N times: retry(3) expression",
	"when":     "Guard clause in automation steps.",
	"as":       "Alias in forEach iteration: for item as alias",
	"where":    "Filter in forEach: for item := range collection where condition",
	"use":      "Import a concept: use v1:domain:concept",
	"concept":  "Define a concept schema: concept Name { ... }",
	"in":       "Membership test: value in collection",
	"has":      "Property existence test: object has \"field\"",
	"not":      "Negation: not in, not has",
}

// FieldTypes lists valid field types for concept definitions.
var FieldTypes = []string{
	"string", "int", "float", "bool", "datetime", "object", "array", "enum",
}
