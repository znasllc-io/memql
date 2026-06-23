package dslspec

// builtins.go is the single source of truth for the memQL expression-level
// builtins -- the bare functions an author calls inside a filter / shape /
// mutation / logic body (concat / coalesce / hash / lower / year / ...), the
// context accessors (item / event / step / now / actor / ...), and the
// runtime-registered builtins resolved from the integration/builtin registry
// (ai / node / similar / embed / ...).
//
// Before #2155 this metadata lived only in a hand-maintained map in
// component/memql/sense/builtins.go (BuiltinFunctions), the one Sense surface
// with no drift guard -- and it had already gone stale against the grammar
// (it omitted year / quarter / month / dayOfMonth / subtractTimestamps /
// isAnniversary / isFirstDayOfQuarter / memqlVersion / contains, all of which
// the parser recognises). This table is the durable fix: Sense is now DRIVEN
// from dslspec.Builtins (sense/builtins.go projects it), and the drift test
// (drift_test.go) pins the grammar-recognised subset to parser.CallableBuiltins
// so the editor can never fall behind the grammar again.
//
// Categorisation (BuiltinCategory) is load-bearing -- the drift test treats the
// three categories differently:
//
//   - CategoryBuiltinExpr: a true editor-callable expression builtin the parser
//     special-cases in parseFunctionCall (parser.CallableBuiltins). The drift
//     test asserts THIS subset's name-set EXACTLY equals parser.CallableBuiltins
//     (both directions).
//   - CategoryBuiltinAccessor: a context accessor the parser recognises
//     (parser.CallableAccessors) -- item / event / step / input / field / var /
//     now / timestamp / actor / error, plus index (special-cased). The reserved
//     ones (now / actor / partition / config / trace) are ALSO modelled by
//     keywords(); they are kept here too because they are callable like
//     functions and Sense offers them in call completion. The drift test asserts
//     this subset is a subset of parser.CallableAccessors (+ the documented
//     index exception).
//   - CategoryBuiltinRegistry: a builtin resolved at runtime from the
//     integration / builtin registry, NOT special-cased by the parser (it falls
//     through parseFunctionCall to a generic FunctionCallExpr): ai / node /
//     children / parent / payload / similar / embed / systemVar / secret /
//     systemSecret. The parser has no grammar rule for these names, so the drift
//     test does NOT pin them to the parser; they are spec-authored metadata
//     Sense needs for completion / hover / signature.

// BuiltinCategory buckets a builtin by how the grammar treats its name, so the
// drift test can pin the parser-recognised expression builtins exactly while
// allowing the accessors and runtime-registry builtins as documented surface.
type BuiltinCategory string

const (
	// CategoryBuiltinExpr is a parser-special-cased expression builtin
	// (parser.CallableBuiltins). Pinned exactly by the drift test.
	CategoryBuiltinExpr BuiltinCategory = "expr"
	// CategoryBuiltinAccessor is a parser-recognised context accessor
	// (parser.CallableAccessors, + index).
	CategoryBuiltinAccessor BuiltinCategory = "accessor"
	// CategoryBuiltinRegistry is a runtime-registry builtin not special-cased
	// by the parser grammar.
	CategoryBuiltinRegistry BuiltinCategory = "registry"
)

// BuiltinParam is one positional/variadic/optional parameter of a builtin, for
// signature help and hover.
type BuiltinParam struct {
	// Name is the parameter label shown in signature help.
	Name string `json:"name"`
	// Doc explains the parameter.
	Doc string `json:"doc"`
}

// Builtin is one expression-level builtin / accessor / registry function in the
// authoring surface.
type Builtin struct {
	// Name is the author-facing spelling (shortId / canonicalId / asOf are
	// mixed-case in source even though the parser lower-cases for dispatch).
	Name string `json:"name"`
	// Category buckets the builtin (see BuiltinCategory).
	Category BuiltinCategory `json:"category"`
	// Signature is the one-line call signature for hover/completion detail,
	// e.g. `concat(values ...string)`.
	Signature string `json:"signature"`
	// Doc is the prose documentation for hover.
	Doc string `json:"doc"`
	// Params lists the parameters for signature help. Empty for nullary
	// accessors (now() / item() / event() / input() / index()).
	Params []BuiltinParam `json:"params,omitempty"`
}

// builtins returns the full editor-callable builtin table. The grammar-
// recognised expression-builtin subset (CategoryBuiltinExpr) is pinned to
// parser.CallableBuiltins by the drift test; the accessor + registry entries
// are the additional editor surface Sense needs.
func builtins() []Builtin {
	return []Builtin{
		// ============================================================
		// Expression builtins (parser.CallableBuiltins) -- pinned exactly.
		// ============================================================
		{
			Name:      "memqlVersion",
			Category:  CategoryBuiltinExpr,
			Signature: `memqlVersion()`,
			Doc:       "Return the running memQL engine version string.",
		},
		{
			Name:      "concat",
			Category:  CategoryBuiltinExpr,
			Signature: `concat(values ...string)`,
			Doc:       "Concatenate multiple string values.",
			Params: []BuiltinParam{
				{Name: "values", Doc: "Strings to concatenate."},
			},
		},
		{
			Name:      "coalesce",
			Category:  CategoryBuiltinExpr,
			Signature: `coalesce(values ...any)`,
			Doc:       "Return the first non-nil value. The final argument is the ultimate fallback and is returned even if empty.",
			Params: []BuiltinParam{
				{Name: "values", Doc: "Values to check, in order."},
			},
		},
		{
			Name:      "cond",
			Category:  CategoryBuiltinExpr,
			Signature: `cond(predicate, thenValue, elseValue)`,
			Doc:       "Return thenValue when predicate is truthy, else elseValue. The canonical conditional-value expression; use the `if` statement for control flow.",
			Params: []BuiltinParam{
				{Name: "predicate", Doc: "Boolean-valued expression."},
				{Name: "thenValue", Doc: "Value returned when predicate is truthy."},
				{Name: "elseValue", Doc: "Value returned when predicate is falsy."},
			},
		},
		{
			Name:      "first",
			Category:  CategoryBuiltinExpr,
			Signature: `first(collection)`,
			Doc:       "Return the first item from a collection.",
			Params:    []BuiltinParam{{Name: "collection", Doc: "Collection to get first item from."}},
		},
		{
			Name:      "last",
			Category:  CategoryBuiltinExpr,
			Signature: `last(collection)`,
			Doc:       "Return the last item from a collection.",
			Params:    []BuiltinParam{{Name: "collection", Doc: "Collection to get last item from."}},
		},
		{
			Name:      "lower",
			Category:  CategoryBuiltinExpr,
			Signature: `lower(value string)`,
			Doc:       "Convert string to lowercase.",
			Params:    []BuiltinParam{{Name: "value", Doc: "String to convert."}},
		},
		{
			Name:      "upper",
			Category:  CategoryBuiltinExpr,
			Signature: `upper(value string)`,
			Doc:       "Convert string to uppercase.",
			Params:    []BuiltinParam{{Name: "value", Doc: "String to convert."}},
		},
		{
			Name:      "trim",
			Category:  CategoryBuiltinExpr,
			Signature: `trim(value string)`,
			Doc:       "Remove leading and trailing whitespace.",
			Params:    []BuiltinParam{{Name: "value", Doc: "String to trim."}},
		},
		{
			Name:      "hash",
			Category:  CategoryBuiltinExpr,
			Signature: `hash(value string)`,
			Doc:       "Compute SHA-256 hash of a string value.",
			Params:    []BuiltinParam{{Name: "value", Doc: "String to hash."}},
		},
		{
			Name:      "shortId",
			Category:  CategoryBuiltinExpr,
			Signature: `shortId(value string)`,
			Doc: "Extract the bare short id from an id-shaped value -- the inverse of canonicalId.\n\n" +
				"Strips the `<partition>:<concept>:` prefix from a canonical node id (e.g. " +
				"`v1:forge:request:r-001` -> `r-001`) and returns the trailing bare slug. " +
				"Idempotent: a value that is already bare (no version-tagged concept prefix) is " +
				"returned unchanged, so calling it on an already-short id is a no-op.\n\n" +
				"Use it to normalize a foreign-key / audit field to one consistent (short) id form " +
				"regardless of whether the caller passes a canonical node id or a bare slug -- e.g. " +
				"`requestId: shortId(args.requestId)` so the audit trail keys consistently across the " +
				"automation (canonical) and tool (short) write paths (#1859).",
			Params: []BuiltinParam{
				{Name: "value", Doc: "Id-shaped value (canonical or bare). Empty input returns empty."},
			},
		},
		{
			Name:      "canonicalId",
			Category:  CategoryBuiltinExpr,
			Signature: `canonicalId(value, concept)`,
			Doc: "Normalize an id-shaped value to canonical form (`<partition>:<concept>:<bareSlug>`).\n\n" +
				"Use in mutation id derivations that hash foreign-key args, so the derived id stays stable whether the caller passes a bare slug or an already-canonical id.\n\n" +
				"Example: `id = concat(\"participant-\", hash(concat(canonicalId(args.partitionId, space), \":\", canonicalId(args.userId, user))))`\n\n" +
				"The second argument is an imported concept short-name (resolved against the file-top `use ...concepts.{ ... }` imports); the stringly-typed `\"v1:ns:name\"` literal is retired. The engine reads the named concept's @scope to pick the right partition prefix (`_system` for global, otherwise the request envelope's partition). Errors when the concept name isn't imported / registered or when the value is already canonical for a different concept (catches type-tag typos).",
			Params: []BuiltinParam{
				{Name: "value", Doc: "Id-shaped value (bare slug or canonical). Empty input returns empty."},
				{Name: "concept", Doc: "Imported concept short-name, e.g. `user` (from `use identity.concepts.{ user }`)."},
			},
		},
		{
			Name:      "toString",
			Category:  CategoryBuiltinExpr,
			Signature: `toString(value any)`,
			Doc:       "Convert a value to its string representation.",
			Params:    []BuiltinParam{{Name: "value", Doc: "Value to convert."}},
		},
		{
			Name:      "addDuration",
			Category:  CategoryBuiltinExpr,
			Signature: `addDuration(timestamp, duration string)`,
			Doc:       "Add a duration to a timestamp (e.g., \"24h\", \"30m\").",
			Params: []BuiltinParam{
				{Name: "timestamp", Doc: "Base timestamp."},
				{Name: "duration", Doc: "Duration string (e.g., \"24h\", \"7d\")."},
			},
		},
		{
			Name:      "daysBetween",
			Category:  CategoryBuiltinExpr,
			Signature: `daysBetween(start, end)`,
			Doc:       "Calculate the number of days between two timestamps.",
			Params: []BuiltinParam{
				{Name: "start", Doc: "Start timestamp."},
				{Name: "end", Doc: "End timestamp."},
			},
		},
		{
			Name:      "subtractTimestamps",
			Category:  CategoryBuiltinExpr,
			Signature: `subtractTimestamps(end, start)`,
			Doc:       "Return the signed duration between two timestamps (end - start) as a duration value.",
			Params: []BuiltinParam{
				{Name: "end", Doc: "End timestamp (minuend)."},
				{Name: "start", Doc: "Start timestamp (subtrahend)."},
			},
		},
		{
			Name:      "year",
			Category:  CategoryBuiltinExpr,
			Signature: `year(timestamp)`,
			Doc:       "Extract the calendar year (integer) from a timestamp.",
			Params:    []BuiltinParam{{Name: "timestamp", Doc: "Timestamp to read the year from."}},
		},
		{
			Name:      "quarter",
			Category:  CategoryBuiltinExpr,
			Signature: `quarter(timestamp)`,
			Doc:       "Extract the calendar quarter (1-4) from a timestamp.",
			Params:    []BuiltinParam{{Name: "timestamp", Doc: "Timestamp to read the quarter from."}},
		},
		{
			Name:      "month",
			Category:  CategoryBuiltinExpr,
			Signature: `month(timestamp)`,
			Doc:       "Extract the calendar month (1-12) from a timestamp.",
			Params:    []BuiltinParam{{Name: "timestamp", Doc: "Timestamp to read the month from."}},
		},
		{
			Name:      "dayOfMonth",
			Category:  CategoryBuiltinExpr,
			Signature: `dayOfMonth(timestamp)`,
			Doc:       "Extract the day of the month (1-31) from a timestamp.",
			Params:    []BuiltinParam{{Name: "timestamp", Doc: "Timestamp to read the day-of-month from."}},
		},
		{
			Name:      "isAnniversary",
			Category:  CategoryBuiltinExpr,
			Signature: `isAnniversary(timestamp, reference)`,
			Doc:       "True when timestamp falls on the same month+day as the reference date (a yearly anniversary match).",
			Params: []BuiltinParam{
				{Name: "timestamp", Doc: "Timestamp to test."},
				{Name: "reference", Doc: "Reference date whose month+day defines the anniversary."},
			},
		},
		{
			Name:      "isFirstDayOfQuarter",
			Category:  CategoryBuiltinExpr,
			Signature: `isFirstDayOfQuarter(timestamp)`,
			Doc:       "True when timestamp is the first day of a calendar quarter (Jan 1 / Apr 1 / Jul 1 / Oct 1).",
			Params:    []BuiltinParam{{Name: "timestamp", Doc: "Timestamp to test."}},
		},
		{
			Name:      "contains",
			Category:  CategoryBuiltinExpr,
			Signature: `contains(haystack string, needle string)`,
			Doc:       "Check if a string contains a substring. (The single-paren `contains(filter)` relationship form is discriminated by arg count.)",
			Params: []BuiltinParam{
				{Name: "haystack", Doc: "String to search in."},
				{Name: "needle", Doc: "Substring to find."},
			},
		},

		// ============================================================
		// Context accessors (parser.CallableAccessors, + index).
		// now / actor / partition / config / trace are ALSO reserved
		// keywords (keywords()); they are listed here too because they are
		// callable like functions and Sense offers them in call completion.
		// ============================================================
		{
			Name:      "now",
			Category:  CategoryBuiltinAccessor,
			Signature: `now()`,
			Doc:       "Return the current timestamp (RFC3339 captured at eval start).",
		},
		{
			Name:      "timestamp",
			Category:  CategoryBuiltinAccessor,
			Signature: `timestamp(value? string)`,
			Doc:       "Parse or return a timestamp. With no arguments, same as now().",
			Params:    []BuiltinParam{{Name: "value", Doc: "Optional timestamp string to parse."}},
		},
		{
			Name:      "error",
			Category:  CategoryBuiltinAccessor,
			Signature: `error(message string)`,
			Doc:       "Return an error, terminating the current execution.",
			Params:    []BuiltinParam{{Name: "message", Doc: "Error message."}},
		},
		{
			Name:      "var",
			Category:  CategoryBuiltinAccessor,
			Signature: `var(name string)`,
			Doc:       "Resolve a partition-scoped plaintext configuration variable from v1:platform:partitionVariable. Falls back to v1:platform:globalVariable (global) if the partition lookup misses.",
			Params:    []BuiltinParam{{Name: "name", Doc: "Variable name (e.g., \"MEMQL_COGNITION_SI_ENABLED\")."}},
		},
		{
			Name:      "step",
			Category:  CategoryBuiltinAccessor,
			Signature: `step(name string)`,
			Doc:       "Reference the result of a previous automation step.",
			Params:    []BuiltinParam{{Name: "name", Doc: "Step identifier."}},
		},
		{
			Name:      "input",
			Category:  CategoryBuiltinAccessor,
			Signature: `input()`,
			Doc:       "Access the automation's input data.",
		},
		{
			Name:      "item",
			Category:  CategoryBuiltinAccessor,
			Signature: `item()`,
			Doc:       "Access the current item in a forEach loop.",
		},
		{
			Name:      "index",
			Category:  CategoryBuiltinAccessor,
			Signature: `index()`,
			Doc:       "Access the current index in a forEach loop. (The no-arg form is the loop accessor; index(arr, i) reads an array element.)",
		},
		{
			Name:      "event",
			Category:  CategoryBuiltinAccessor,
			Signature: `event()`,
			Doc:       "Access the trigger event data in an automation.",
		},
		{
			Name:      "field",
			Category:  CategoryBuiltinAccessor,
			Signature: `field(name string)`,
			Doc:       "Reference a field accessor in the current evaluation context.",
			Params:    []BuiltinParam{{Name: "name", Doc: "Field name to access."}},
		},
		{
			Name:      "actor",
			Category:  CategoryBuiltinAccessor,
			Signature: `actor()`,
			Doc:       "Access the resolved auth context (userId, role, identityId, isClusterOwner, partitions).",
		},

		// ============================================================
		// Runtime-registry builtins (NOT special-cased by the parser; resolved
		// from the integration / builtin registry at runtime). Not pinned to
		// the parser by the drift test.
		// ============================================================
		{
			Name:      "ai",
			Category:  CategoryBuiltinRegistry,
			Signature: `ai(templateId string, data object, provider? string)`,
			Doc:       "Invoke an AI prompt template with the given data.",
			Params: []BuiltinParam{
				{Name: "templateId", Doc: "Name of the prompt template to invoke."},
				{Name: "data", Doc: "Data object passed to the template."},
				{Name: "provider", Doc: "Optional provider override (e.g., \"claudeSonnet\")."},
			},
		},
		{
			Name:      "node",
			Category:  CategoryBuiltinRegistry,
			Signature: `node(field string)`,
			Doc:       "Access a field from the current node in a shape template.",
			Params:    []BuiltinParam{{Name: "field", Doc: "Dot-separated field path (e.g., \"payload.name\")."}},
		},
		{
			Name:      "children",
			Category:  CategoryBuiltinRegistry,
			Signature: `children(concept string)`,
			Doc:       "Retrieve child nodes of the current node for a given concept.",
			Params:    []BuiltinParam{{Name: "concept", Doc: "Child concept name (e.g., \"v1:cognition:participant\")."}},
		},
		{
			Name:      "parent",
			Category:  CategoryBuiltinRegistry,
			Signature: `parent(concept string)`,
			Doc:       "Retrieve the parent node for a given concept.",
			Params:    []BuiltinParam{{Name: "concept", Doc: "Parent concept name."}},
		},
		{
			Name:      "payload",
			Category:  CategoryBuiltinRegistry,
			Signature: `payload(field string)`,
			Doc:       "Access a field from the event payload.",
			Params:    []BuiltinParam{{Name: "field", Doc: "Dot-separated field path."}},
		},
		{
			Name:      "similar",
			Category:  CategoryBuiltinRegistry,
			Signature: `similar(text string, concept string, field string, topK? int, minScore? float)`,
			Doc:       "Find semantically similar nodes using vector search (pgvector).",
			Params: []BuiltinParam{
				{Name: "text", Doc: "Query text to find similar content for."},
				{Name: "concept", Doc: "Concept to search within."},
				{Name: "field", Doc: "Vector field name (e.g., \"content\")."},
				{Name: "topK", Doc: "Maximum number of results (default: 10)."},
				{Name: "minScore", Doc: "Minimum similarity score 0-1 (default: 0.5)."},
			},
		},
		{
			Name:      "embed",
			Category:  CategoryBuiltinRegistry,
			Signature: `embed(text string, model? string)`,
			Doc:       "Generate an embedding vector for text using the configured provider.",
			Params: []BuiltinParam{
				{Name: "text", Doc: "Text to embed."},
				{Name: "model", Doc: "Optional model override (default: text-embedding-3-small)."},
			},
		},
		{
			Name:      "systemVar",
			Category:  CategoryBuiltinRegistry,
			Signature: `systemVar(name string)`,
			Doc:       "Resolve an instance-wide (global) plaintext configuration variable from v1:platform:globalVariable. No fallback.",
			Params:    []BuiltinParam{{Name: "name", Doc: "System variable name (e.g., \"MEMQL_DEFAULT_CHAT_PROVIDER\")."}},
		},
		{
			Name:      "secret",
			Category:  CategoryBuiltinRegistry,
			Signature: `secret(name string)`,
			Doc:       "Resolve a partition-scoped encrypted secret from v1:platform:partitionSecret. Falls back to v1:platform:globalSecret (global) if the partition lookup misses. Returns the decrypted plaintext; requires MEMQL_MASTER_KEY. Callers must never log the result.",
			Params:    []BuiltinParam{{Name: "name", Doc: "Secret name (e.g., \"MEMQL_OPENAI_API_KEY\")."}},
		},
		{
			Name:      "systemSecret",
			Category:  CategoryBuiltinRegistry,
			Signature: `systemSecret(name string)`,
			Doc:       "Resolve an instance-wide (global) encrypted secret from v1:platform:globalSecret. No fallback. Returns the decrypted plaintext; requires MEMQL_MASTER_KEY.",
			Params:    []BuiltinParam{{Name: "name", Doc: "System secret name (e.g., \"MEMQL_IDENTITY_KEY_ENCRYPTION_KEY\")."}},
		},
	}
}
