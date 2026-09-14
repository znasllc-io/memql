// Package annotations is the single physical source of truth for the
// MemQL construct-annotation surface: which annotations each receiver
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
		// @enabled, @latestMode and @nocache retired in memql#5375: the
		// first was an explicit no-op, the second restated what `asOf
		// latest` in the body already says, the third was the second
		// spelling of @cache(0).
		"description", "disabled", "public", "serverOnly", "mcp", "unbounded", "cache", "actor", "requiresRank", "requiresCapability",
	},
	"Mutation": {
		"description", "disabled", "actor", "public", "serverOnly",
		"mergeFields", "appendFields", "addToSet", "removeFromSet", "createOnly", "noUnset", "scrubPii", "mcp", "requiresRank", "requiresCapability",
	},
	"Logic": {
		"description", "disabled", "eventField", "actor", "requiresRank", "requiresCapability",
	},
	"Automation": {
		// @schedule retired in memql#5375: @trigger(schedule=...) is the
		// one spelling, which keeps "how is this automation reached" a
		// single annotation and lets @template be refused beside it
		// coherently.
		"description", "disabled", "trigger", "filter", "mcp", "actor", "template",
	},
	"Action": {
		"description", "disabled", "kind", "sideEffect",
	},
	"Capability": {
		"description", "disabled", "sideEffect",
	},
	"Spec": {
		"description", "disabled",
	},
	"Tool": {
		// @rateLimit and @scopes retired in memql#5375: both were stored
		// on the tool, cloned, advertised on the gRPC tool descriptor and
		// enforced nowhere -- so each read as a ceiling or a gate while
		// being neither, which is worse than their absence.
		//
		// @allowedRoles STAYS. D17 proposed replacing it with
		// @requiresRank + @requiresCapability, but those gate the human
		// actor's catalog rank and their grants over a resource, while
		// this gates which AGENT role may call the tool -- a different
		// axis, enforced on every path (tool_types.go, grpc/server.go,
		// tool_execution.go). Substituting rank for agent role would let
		// every specialist call the assistant-only tools.
		"description", "disabled", "handler", "executionTime",
		"destructive", "requiresConfirmation",
		"allowedRoles", "mcp",
	},
	"Builtin": {
		// @requiresCapability (epic memql#5288, task memql#5301): a builtin is
		// where most of an app's ACTIONS live -- packageDeploy, siteArchive,
		// customDomainAdd are all builtins -- so the part vocabulary
		// (`execute` on `app:<id>/<part>`) has to be declarable here or it
		// gates nothing that matters. @requiresRank stays off builtins: a
		// rank floor on a Go-served read is applied in its handler (the
		// logsSearch precedent), and nothing has asked for the annotation.
		"description", "disabled", "executor", "alias", "args", "sdk",
		"requiresCapability",
	},
	"Prompt": {
		"description", "disabled", "level", "defaultProvider", "templateFile",
	},
	"Provider": {
		// @type renamed @vendor in memql#5375: one name, one meaning. A
		// concept's @type is its row kind, and the two shared a spelling
		// for no reason beyond history.
		"disabled",
		"description", "vendor", "model", "modality", "default", "base", "extends",
	},
	"Shape": {
		"description", "row", "actor",
	},
	"Policy": {
		"description", "primary", "fallback",
	},
	"Rule": {
		"description", "disabled",
		"when", "policy", "level", "precedence", "onUnavailable", "exclude", "locked",
	},
	"Seed": {
		// @namespace retired in memql#5375; @version stays -- a seed's
		// version is id-bearing.
		"description", "version", "scope", "templateFile", "disabled",
	},
	"": { // top-level (concept definitions)
		// @namespace retired in memql#5375: the namespace is the domain
		// directory or that directory's one-line namespace.pin, so the
		// annotation could only restate one or disagree with it.
		// @version stays -- it is the "v1" of every canonical id.
		//
		// @displayCard and @composable are declared here as well as in
		// component/database/memory-nodes' own concept table, because
		// memql#5378 verified both are READ (see Docs, which names the
		// reader) and the attribute matrix derives its rows from this map.
		"description", "version", "scope", "visibility", "type", "cache", "relationship",
		"rowAuthz", "origin", "mirroredTo", "displayCard", "composable",
	},
}

// Docs maps an annotation name to its one-line hover/completion doc.
// Every name offered by ByReceiver carries an entry here (enforced by
// TestEveryAnnotationHasDoc in component/memql/sense).
var Docs = map[string]string{
	// Lifecycle / shared.
	"disabled":           "Disable this definition.",
	"description":        "Human-readable description of this definition. PREFER the /// doc-comment form (#2601): a /// block immediately above the declaration IS the description and wins over this annotation; @description remains the valid compatibility fallback -- the tree gate rejects the redundant long form (including a bare @description shadowed by a /// block). Aim for ~500 characters (editorial target).",
	"eventField":         "On an event-triggered logic: declare the allowed top-level event payload fields (e.g. @eventField(\"partitionId\", \"siParticipantId\")). Opt-in field-level validation -- every event.payload.<field> reference in the body is checked against this set at load time, rejecting typos / fields the (possibly synthetic, handler-assembled) triggering event cannot carry (memql#1743). Bare names or payload.-prefixed paths both normalize to the head segment.",
	"public":             "Per-row-authz marker: this query/mutation is intentionally callable without a caller-scope filter (concept catalogs, pre-auth login paths). See docs/public/operate/auth/per-row-authz-audit.md.",
	"requiresCapability": "The CAPABILITY a caller must hold to invoke the construct -- @requiresCapability(\"read\", \"principal\") (epic memql#5166, D11). The sibling of @requiresRank, and the difference is the point: a RANK is a floor on the cluster's ladder, a CAPABILITY is a (verb x resource) grant a role was given, and a cluster can hold one without the other -- developer ranks above admin and holds strictly fewer principal verbs. Declared together, BOTH must pass. VALIDATED AT LOAD against the five verbs and the resource kinds any role in this cluster holds a grant on, so a misspelling refuses boot rather than gating a surface into silence; ENFORCED at execution through the runtime capability catalog, on the direct call and on every plan that expands the construct. It replaces the slug-comparing specs (requiresAdmin, requiresOwnerOrAdmin, requiresDeveloperOrAbove), which could not see a custom role at all: `role == \"admin\"` is false for a rank-250 role holding every principal verb. It gates WHO MAY CALL; @rowAuthz still decides WHICH ROWS come back.",
	"requiresRank":       "The actor-rank FLOOR: only a caller holding this role, or one ranked above it, may invoke the construct -- @requiresRank(\"developer\") (epic memql#4832, D6). ENFORCED at execution and VALIDATED AT LOAD against the role ladder in dsl/rbac, so a typo refuses boot rather than gating on rank 0 and admitting everyone. This is the server-side counterpart to MemQL OS's per-surface role requirement: the shell keeps hiding what a caller cannot reach (hiding an action beats letting them click it and reading a refusal) and this makes the hidden surface a REFUSED one. Declared on the CONSTRUCT because a surface is a set of constructs and an app id from a browser is a claim, not a fact. It gates WHO MAY CALL; @rowAuthz still decides WHICH ROWS come back.",
	"serverOnly":         "Bars the construct from client-originated calls while leaving server-side Go free to call it (memql#2800). ENFORCED at execution against auth.CallOrigin -- unlike the retired @internal, which only hid a construct from discovery. Use only when caller-scoping is impossible: the auth path resolving `sub` -> user before an actor exists, or an automation acting on a user other than the actor. Callers must stamp auth.ContextWithInternalOrigin.",
	"actor":              "On a mutation: resolves auth-context (`actor.X`) fields. On a shape: kind marker -- projects the auth-context envelope (actor.userId / role / ...).",
	"mergeFields":        "On an update mutation: deep-merge the named object-typed payload fields into the stored object instead of replacing them wholesale, so sibling keys survive a single-key write. Format: @mergeFields(\"preferences\").",
	"appendFields":       "On an update mutation: append the named array-typed payload fields' elements to the stored array instead of replacing it wholesale, so a single-writer mutation can accumulate list items (e.g. attach one id). Format: @appendFields(\"attachmentIds\").",
	"addToSet":           "On an update mutation: treat the named array-typed payload fields as SETS and UNION the written elements into the stored array -- deduped, existing order kept, new members appended in the order given. The membership half @appendFields is not: append is not deduped and has no counterpart that removes, so a toggle built on it duplicates on a double click. Pairs with @removeFromSet. Format: @addToSet(\"disabledDeployables\"). See memql#4951.",
	"removeFromSet":      "On an update mutation: treat the named array-typed payload fields as SETS and REMOVE the written elements from the stored array, keeping the order of what remains. Removing something absent is a no-op rather than an error, so the mutation is idempotent and two callers removing the same member both succeed. Pairs with @addToSet, and the pair is what lets a set be edited one member at a time instead of read-modify-written whole. Format: @removeFromSet(\"disabledDeployables\"). See memql#4951.",
	"createOnly":         "On an insert (create-or-upsert) mutation: write the named payload fields ONLY when creating the row. If the target id already exists, the fields are dropped from the delta before the engine read-merge, so the stored value is preserved rather than clobbered -- making a deterministic-id re-stage idempotent for lifecycle fields another writer owns after creation (e.g. stageOutboundRequest seeds status but must not reset a row the outbound worker moved to sent). The inverse of @mergeFields/@appendFields: only valid on insert-kind mutations. Format: @createOnly(\"status\", \"attempts\"). See fylo#63.",
	"noUnset":            "On any mutation: declare the named payload fields ONE-WAY -- a write may set them or change one non-empty value to another, but may never take a stored non-empty value back to empty. On the read-merge path a named field arriving empty is dropped from the delta when the stored row holds a non-empty value. Closes the gap read-merge cannot (it only inherits fields ABSENT from a delta, so a body writing `f: args.f ?? \"\"` blanks the stored value with an explicit empty string). Distinct from @createOnly, which forbids any post-create write; @noUnset forbids only set -> unset, so a legitimately-later stamp still lands. Format: @noUnset(\"bootstrappedAt\"). See memql#3415.",
	"scrubPii":           "On an update mutation (the hard-delete / data-deletion path): after the partial payload merges, zero EVERY field the bound concept marks @pii. The field set is derived from the schema, so a newly-annotated PII field is scrubbed automatically with no change to the mutation. Bare flag, no arguments. See memql#1711.",
	// Automation.
	"trigger":  "How an automation is reached: @trigger(event=\"graph.node.created.*.v1:ns:concept\") for the graph, or @trigger(schedule=\"0 0 * * * *\") for the clock. ONE annotation for both, which is what keeps \"triggered\" and \"called\" distinguishable from @template. The separate @schedule(cron=...) spelling is retired in memql#5375.",
	"filter":   "Filter expression for automation triggers.",
	"template": "On an automation: this is a work-spine TEMPLATE, invoked by a v1:work:run that named it rather than fired by the graph (memql#5048). It is the third way an automation can be reachable, alongside an event trigger and a schedule. A @template automation must carry NEITHER @trigger nor @schedule -- the load-time gate refuses both combinations, so \"called\" and \"triggered\" stay distinct.",
	// Action (memql#2218, behavioral-constructs ADR §2.3).
	"kind":       "On an action: the action kind. @kind(\"primitive\") = one external capability rendered from params (the only kind today; composites collapse into automations, ADR §2.2).",
	"sideEffect": "On an action: coarse risk class @sideEffect(\"read\"|\"write\"|\"exec\") carried for authoring + the surface-aware trust gate. The AUTHORITATIVE sideEffectClass lives on the capability (ADR §7) so an authored/generated action cannot spoof it.",
	// Pagination opt-out (epic 5, memql#1965).
	"unbounded": "On a list-returning query: opt out of the pagination authoring rule and the implicit 50-row runtime cap. Format: @unbounded(\"reason\"). The reason string is REQUIRED -- it documents why this query is a legitimate full-set read (small bounded catalog, sweep job, etc.) and is enumerated by the pagination audit report. A query that paginates/sorts is already bounded and must NOT carry @unbounded; the engine clamps the realized window to MEMQL_MEMORY_ENGINE_MAX_WINDOW regardless. See docs/public/language/authoring-rules.md.",
	// Temporal-access visibility (core-builtins ADR §2.3, memql#2305).
	// MCP promotion (epic memql#1529 Phase 4 #1534).
	"mcp": "Expose this construct on the MCP connector surface. On a query/mutation/automation it promotes the construct into its own first-class MCP tool (otherwise it stays reachable via the generic run_query / run_mutation / run_automation dispatchers). On a tool it opts the tool into the curated connector allowlist: once ANY tool carries @mcp, tools/list reflects only @mcp tools (otherwise -- zero tagged -- the full tool surface is reflected, so the annotation is inert until the curated set is tagged).",
	// Tool.
	"handler":              "Tool handler configuration. Format: @handler(type=\"query\", query=\"...\") / @handler(type=\"function\", name=\"...\").",
	"executionTime":        "Expected execution time hint: \"fast\", \"medium\", or \"slow\".",
	"destructive":          "Mark a tool as destructive (mutates/deletes); the tool loop gates it behind a confirmation.",
	"requiresConfirmation": "Require explicit user confirmation before the tool executes.",
	"allowedRoles":         "Restrict the tool to a set of AGENT roles: @allowedRoles(\"assistant\", \"specialist\"). An EMPTY list means every agent role. ENFORCED on every path -- the predicate is Tool.AllowedRoles in component/memql/tool_types.go, applied by component/grpc/server.go before dispatch and by tool_execution.go on the in-engine path. This is the agent-role axis, NOT the actor's catalog rank: @requiresRank is a floor on the human caller and @requiresCapability is a grant over a resource, and neither can say \"a specialist may not call this\". memql#5375 proposed replacing it with those two, re-verified it live, and kept it for that reason.",
	// Builtin.
	"executor": "Go executor name for builtin functions (integration.X.Y).",
	"args":     "Parse-time argument contract for builtin functions.",
	"alias":    "Additional name the builtin is registered under.",
	"sdk":      "Generator marker (sdk/gen reads from source); no engine effect.",
	// Prompt.
	"defaultProvider": "Default AI provider for prompt execution.",
	"templateFile":    "External template file path for prompts.",
	// Provider.
	"type":     "On a CONCEPT: the row kind -- \"object\" (default), \"collection\" or \"reference\". Drives the collection/reference node-type invariants. A provider's vendor is @vendor, renamed off this spelling in memql#5375 so one name means one thing.",
	"model":    "Model identifier (e.g., \"gpt-5.4-mini\", \"claude-sonnet-4-6\").",
	"modality": "Provider modality (e.g., \"chat\", \"audio\", \"image\", \"embedding\").",
	"default":  "On a PROVIDER: mark it the default for its modality. On a TOOL, PROMPT or BUILTIN field: the JSON-Schema default the model reads when the caller omits the field -- those bodies ARE the schema, which is why it survives there. RETIRED on a concept field and on an args field (memql#5375): neither was ever applied on insert, so a field carrying it did not default; fill the value with `??` in the mutation that writes it.",
	"base":     "Mark a vendor-level base provider (auth + type only).",
	"extends":  "Inherit configuration from a base provider.",
	// Shape.
	"row": "Shape kind: projects a concept's payload + row intrinsics (concept bound via the `shape <Concept> <name>` signature).",
	// Policy (AI Router provider-selection records). @primary is required; @fallback is repeatable.
	// Every entry is held to the closed entry grammar: a bare provider name,
	// fleet:strongest / fleet:fastest / fleet:<modelId>, app:* / app:<id>,
	// federation:cheapest / federation:strongest / federation:<providerName>, or policy:<name>.
	"primary":  "Required on a policy: the first entry the AI Router resolves. A provider name, a fleet: / app: / federation: selector, or policy:<name>.",
	"fallback": "On a policy: an entry tried when the ones before it cannot serve the call. Repeatable (order preserved). Same closed grammar as @primary.",
	// Rule (the call-metadata -> policy mapping the router evaluates in precedence order).
	"when":          "On a rule: the condition set, as keyword arguments. Closed keys, every one optional, all present keys ANDed: level, modality, prompt, role, actorRole, tag, touches. @when() with no arguments matches every call.",
	"policy":        "Required on a rule: the policy this rule resolves the call through.",
	"level":         "On a prompt: how much intelligence the call needs -- fast, strong, reasoning or embeddings. On a rule: the level to resolve at, OVERRIDING what the call declared.",
	"precedence":    "On a rule: evaluation order, highest first. A tie between two rules of the same locked-ness is a load error.",
	"onUnavailable": "On a rule: what happens when the chain is exhausted at the level -- \"degrade\" walks down to the next level and records that it did, \"park\" returns the refusal with the door report. Empty reads as degrade.",
	"exclude":       "On a rule: remove one concrete entry from the chain's resolution, e.g. @exclude(\"fleet:qwen3.5:7b\"). Repeatable.",
	"locked":        "On a rule: evaluate before every unlocked rule regardless of precedence. Accepted only in the embedded tree -- the loader refuses it elsewhere.",
	// Concept.
	"version":    "Version tag for a concept.",
	"scope":      "Partition scope. @scope(\"global\") places rows in the reserved _system partition; default is partition-scoped.",
	"visibility": "Which node types load this concept. @visibility(\"*\"), @visibility(\"cognition\", \"bff\"), or @visibility(!\"planner\").",
	"cache":      "Override the result-cache TTL for the query, in whole seconds: @cache(300). @cache(0) is the explicit \"never cache\" -- use it for reads where even brief staleness is wrong (auth, monotonic counters, presence). Pure reads cache BY DEFAULT with a 60s backstop, so @cache sets a DIFFERENT TTL rather than turning caching on. The keyword form @cache(ttl=\"300\") and the @nocache alias are both retired in memql#5375: one value, one spelling, and a single-arg annotation has no ambiguity for a keyword to resolve. The engine keys the cache on the plan signature (query/sort/limit/depth/shape + the keyset cursor) and evicts on any write to the read concept via the cache.invalidate.* broadcast channel, so cross-node eviction needs no per-concept routing rule.",
	// Two independent axes: `type` is what the ENGINE DOES with the edge (a
	// closed set), `as` is what the edge MEANS (open, form-validated only).
	// The target is a BARE concept name resolved through a file-top `use`
	// import -- the quoted canonical-ID form this doc used to show was retired
	// by memql#1067, so the editor was teaching an author the one spelling the
	// conformance gate rejects (memql#3661).
	"relationship": "Foreign-key relationship metadata. Format: @relationship(type=\"parent\", field=\"x\", target=concept, direction=\"outgoing\"), plus an optional as=\"domainVerb\" label.",
	"rowAuthz":     "Declares WHO MAY SEE this concept's rows, once on the concept, instead of as an `actor.*` term every filter over it must remember to carry. Four tiers, one spelling each: @rowAuthz(public) (globally readable by intent -- spelled explicitly, because \"no annotation\" and \"declared public\" are different states), @rowAuthz(clusterOwner) (administrative), @rowAuthz(owner=\"<field>\") (the field is compared against actor.userId; it must be a field the concept declares, OR the literal \"id\" for a SELF-OWNED concept whose owner is the row itself -- memql#3029; `id` and only `id`, since createdBy means who WROTE the row, not whose row it is), @rowAuthz(via=\"<spec>\") (a relationship spec grants visibility). A fifth FORM, not a fifth tier: @rowAuthz(owner=\"<field>\", clusterOwner) is the composite -- the owner, OR a cluster owner (memql#4312) -- the only two-argument list, order-independent, and the form an operator console needs over per-user rows since a plain owner= tier has no cluster-owner bypass. ENFORCED ON THE READ PATH since Phase 3 (memql#3172): declaring a tier CHANGES WHAT READS RETURN. Two mechanisms, and neither consults the filter to decide whether to engage -- the tier's predicate is ANDed into the plan before the read runs (resolved from the construct's declared binding, so the narrowing pushes down into SQL), and every row leaving the engine is separately admitted against the tier ITS OWN concept declares, which is the only mechanism available to a raw client-supplied query string, to graph expansion, or to a TOP-LEVEL BUILTIN CALL whose rows come out of a Go handler (memql#3982) -- none of which has a filter to AND anything into. SUBSCRIPTIONS are gated by the same row admission (memql#4309): a graph.node.* event reaches a stream only if the tier admits the row for that stream's actor, a `granted` row arrives id-only with payload_omitted set for the client to re-read, and an UNDECLARED concept is delivered to everyone exactly as its reads already return to everyone -- the live feed mirrors the read path rather than running a second rulebook. The write side is enforced too: update/delete refuse when the target row's declared owner is not the actor (memql#3174). Implementation: component/memql/rowauthz_enforce.go, called from parser.go. MEASURED BY TestClusterOwnerTierInjectsTheAdminGate, TestFilteredReadPathAppliesTheRowGate, TestGraphExpansionAppliesTheTraversalGateBeforeItEmitsTheRow TestTopLevelBuiltinAppliesTheRowGate and TestSubscriptionFanOutAppliesTheRowGate -- named so a reader can check whether this is still true rather than trust the sentence. Trusting it would have been wrong before: this paragraph described the tier as parsed-but-unread for as long as Phase 3 had been live, which is false in the one direction that costs a reader a wrong authorization assumption, and it feeds editor hover, so the reach was wider than this file (memql#3727). See docs/public/operate/auth/per-row-authz-audit.md and memql#2803.",
	// Data origins (epic memql#4378). Two declarations, three derived
	// states, no fourth.
	"origin": "Declares WHERE CHANGES TO THIS CONCEPT ARE MADE -- the system that owns the data. @origin(\"memql\") (the default when the annotation is absent) means MemQL originates it; @origin(\"<connector>\") names an external system, which makes the concept a MIRROR. A mirror is READ-ONLY BY CONSTRUCTION: component/memql refuses every write to it -- mutation, tool handler, raw insert or staged write -- that does not come from the connector the origin names, so what the badge says is what a reader may assume. The name must be a registered connector or the engine REFUSES BOOT naming the concept: a mirror nobody fills is a lie. Pairs with @mirroredTo to derive dataState (mirror | origin | native), which the registry, both SDKs and the portal badge read. See docs/public/concepts/data-origins.md.",
	"vendor": "The AI vendor a provider speaks to: @vendor(\"OpenAI\") or @vendor(\"Anthropic\"). Renamed from @type in memql#5375 so one name means one thing -- a concept's @type is its row kind, and the two shared a spelling for no reason beyond history. Declared on a @base provider; a child @extends the base and inherits it.",

	// The two concept annotations memql#5378 verified are READ, declared
	// here beside the rest. Each doc NAMES its reader, so the next audit
	// can check whether it is still true rather than re-deriving the grep.
	"displayCard": "Per-concept rendering hints for concept-agnostic surfaces: @displayCard(primary=\"name\", secondary=\"role\", tertiary=\"ownerUserId\", status=\"active\"). Each slot names a declared property or a row intrinsic. READ BY clients/os -- src/apps/concepts/displayCard.ts resolves the slots and RowsPanel.tsx renders them, so a concept with no card shows its id and nothing else. Every concept must declare one or decline it with a `// @no-displayCard: <reason>` comment (test/dslconformance/displaycard_inventory_test.go). Verified still read in memql#5378, which is why D17's retirement of it was not carried out.",
	"composable":  "Marks a concept available to the Materializer's composer, optionally naming the composable fields: @composable(as=\"invoice\", fields=\"number,issuedAt,total\"). Every named field must be a declared property or a row intrinsic (validateComposable). READ BY clients/os -- served through integration.compose.composableConcepts and read by src/apps/materializer/useCompose.ts, so retiring it would empty the composer's list. ABSENT means not composable, which is a different statement from composable-with-no-fields (epic memql#4977, D2). Verified still read in memql#5378.",

	"mirroredTo": "Declares WHO ELSE HOLDS A COPY of this MemQL-origin concept: @mirroredTo(\"shopify\"), or several names in one annotation. Every write to the concept appends one v1:platform:outboxEntry per target, in the write's own transaction, which a per-connector drain worker delivers with an idempotency key, backoff, dead-lettering and audit. Only valid on a MemQL-origin concept -- @mirroredTo beside an external @origin is REFUSED at load, because re-mirroring somebody else's data onward is the origin's job and a mirror that also publishes is a second origin wearing the first one's badge. Each named connector must be registered or the engine refuses boot: a mirror target nobody drains is a silent drop. See docs/public/concepts/data-origins.md.",
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

// ArgSpec is one keyword argument an annotation accepts inside @name(...).
type ArgSpec struct {
	Name string // keyword-arg name, e.g. "event"
	Type string // "string", "int", ...
	Doc  string // one-line completion/hover doc
}

// KeywordArgs maps an annotation name to the keyword arguments its
// parenthesized form accepts, i.e. @name(arg=value, ...). It is the source of
// truth for the editor's completion inside an annotation's argument list, so
// the Sense surface no longer hand-maintains its own copy. Only annotations
// whose form is a set of key=value pairs appear here; single-value annotations
// (@description("..."), @model("...")) and flag annotations (@enabled) do not.
// Every key must be an annotation the ByReceiver/Docs surface knows (guarded by
// a test in component/memql/sense).
var KeywordArgs = map[string][]ArgSpec{
	"trigger": {
		{Name: "event", Type: "string", Doc: "Event pattern, e.g. \"graph.node.created.*.v1:ns:concept\"."},
		{Name: "schedule", Type: "string", Doc: "Cron schedule, e.g. \"0 0 * * * *\"."},
		{Name: "concept", Type: "string", Doc: "Concept id the triggering event targets."},
		{Name: "partition", Type: "string", Doc: "Partition selector, e.g. \"*\" for all partitions. Required while the event topic carries a partition segment (#56 phase 8)."},
	},
	"handler": {
		{Name: "type", Type: "string", Doc: "Handler type: \"query\", \"function\", or \"webhook\"."},
		{Name: "query", Type: "string", Doc: "MemQL query expression (with type=\"query\")."},
		{Name: "name", Type: "string", Doc: "Function name (with type=\"function\")."},
	},
	"when": {
		{Name: "level", Type: "string", Doc: "The level the call declared: fast, strong, reasoning or embeddings."},
		{Name: "modality", Type: "string", Doc: "The modality derived from the call site: chat, streamingChat, tools, streamingTools, structured, vision, embedding, speech, transcribe."},
		{Name: "prompt", Type: "string", Doc: "The DSL prompt this call renders. Empty matches a Go call site with no prompt."},
		{Name: "role", Type: "string", Doc: "The AGENT's role slug. Distinct from actorRole: this is what is acting."},
		{Name: "actorRole", Type: "string", Doc: "The calling human's cluster role. Distinct from role: this is who is watching."},
		{Name: "tag", Type: "string", Doc: "A call tag, e.g. \"background\" or \"backgroundEscalation\"."},
		{Name: "touches", Type: "string", Doc: "A concept id PREFIX the call's footprint matches (startsWith semantics)."},
	},
	"displayCard": {
		{Name: "primary", Type: "string", Doc: "Property or row intrinsic for the card's first line."},
		{Name: "secondary", Type: "string", Doc: "Property or row intrinsic for the card's second line."},
		{Name: "tertiary", Type: "string", Doc: "Property or row intrinsic for the card's third line."},
		{Name: "status", Type: "string", Doc: "Property whose value renders as the card's status chip."},
	},
	"composable": {
		{Name: "as", Type: "string", Doc: "Singular label the composer offers this concept under."},
		{Name: "fields", Type: "string", Doc: "Comma-separated declared properties or row intrinsics the composer may pull."},
	},
	"relationship": {
		{Name: "type", Type: "string", Doc: "STRUCTURAL type -- what the engine does with the edge. Closed set: parent, owns, createdBy, alias, equals, contains, references."},
		{Name: "field", Type: "string", Doc: "Local field holding the foreign key."},
		{Name: "target", Type: "string", Doc: "Target concept id, e.g. \"v1:ns:concept\"."},
		{Name: "direction", Type: "string", Doc: "\"outgoing\" or \"incoming\"."},
		{Name: "as", Type: "string", Doc: "DOMAIN label -- what the edge means, e.g. as=\"assignedTo\". Optional. Any lowerCamelCase identifier; validated for form only and never checked against a list, so a new verb never needs an engine release (memql#3652)."},
	},
}

// KeywordArgsFor returns the keyword arguments an annotation accepts inside its
// @name(...) form, or nil when it takes none (or takes a single value rather
// than keyword pairs).
func KeywordArgsFor(name string) []ArgSpec {
	return KeywordArgs[name]
}
