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

// AnnotationsByReceiver maps receiver types to their valid annotations.
var AnnotationsByReceiver = map[string][]string{
	"Query": {
		"enabled", "description", "version", "internal",
	},
	"Mutation": {
		"enabled", "description", "version", "internal",
	},
	"Automation": {
		"enabled", "description", "trigger", "filter", "version",
	},
	"Spec": {
		"enabled", "description", "version",
	},
	"Tool": {
		"enabled", "description", "handler", "executionTime",
	},
	"Builtin": {
		"enabled", "description", "executor", "args", "version", "internal",
	},
	"Prompt": {
		"enabled", "description", "defaultProvider", "templateFile", "version",
	},
	"Provider": {
		"type", "model", "extends", "description",
	},
	"Shape": {
		"enabled", "description", "concepts", "version",
	},
	"": { // top-level (concept definitions)
		"description", "version", "namespace", "scope", "visibility", "type",
	},
}

// AnnotationDocs maps annotation names to documentation strings.
var AnnotationDocs = map[string]string{
	"enabled":         "Enable or disable this definition. Functions are disabled by default.",
	"description":     "Human-readable description of this definition.",
	"version":         "Version tag for this definition.",
	"internal":        "Hide from external API discovery.",
	"trigger":         "Event trigger for automations. Format: @trigger(event=\"graph.node.created.*\")",
	"filter":          "Filter expression for automation triggers.",
	"handler":         "Tool handler configuration. Format: @handler(type=\"query\", query=\"concept==\")",
	"executionTime":   "Expected execution time hint: \"fast\", \"medium\", or \"slow\".",
	"executor":        "Go executor name for builtin functions.",
	"args":            "Parse-time argument contract for builtin functions.",
	"defaultProvider": "Default SI provider for prompt execution.",
	"templateFile":    "External template file path for prompts.",
	"type":            "Provider type (e.g., \"OpenAI\", \"Anthropic\").",
	"model":           "Model identifier (e.g., \"gpt-5-mini\", \"claude-sonnet-4-6\").",
	"extends":         "Inherit configuration from a base provider.",
	"namespace":       "Concept namespace. Colon-separated lowercase identifiers (e.g., \"cognition\" or \"cognition:client:tool\").",
	"scope":           "Partition scope. @scope(\"global\") places rows in the reserved _system partition; default is partition-scoped.",
	"visibility":      "Which node types load this concept. @visibility(\"*\"), @visibility(\"cognition\", \"bff\"), or @visibility(!\"planner\").",
	"concepts":        "List of concepts this shape applies to.",
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
