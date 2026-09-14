package annotations

// long_docs.go holds the docs that run to paragraphs. Each is a reader-facing
// rule that used to live only in a hand-written attribute matrix; since that
// page is generated from this registry (docs/public/language/attribute-matrix.md,
// memql#5360), the rule lives here or nowhere. A placement's Doc feeds the
// page, while hover and completion keep reading the one-line Docs[name], so
// the paragraphs below reach the page without lengthening a tooltip -- except
// the few Docs entries defined here (docRowAuthz, docAddToSet,
// docRemoveFromSet, docNoUnset), which both read.
//
// Every claim below was checked against the code that makes it true, and the
// comment above each constant names that code, so the next reader can check
// it again. When the code moves, change the doc here and run
// `make docs-matrix`.

// docDisabled is @disabled on every construct that takes it (one doc, so the
// page prints it once). The paths: functions() and help() in
// component/memql/executor_builtin.go; MCP promotion and function-backed tool
// registration in mcp_promote.go and function_tools.go; the call refusal in
// engine.go and function_validator.go; the builtin executor check in
// executor_builtin.go; automations in automations/scheduler.go, run_relay.go,
// server/server.go and app/mcp_automation_runner.go; the load skips in
// unified_kinds_loader.go (tool, seed, prompt, provider), unified_spec_loader.go
// and spec_converter.go (spec, trait), actions/loader.go (action) and
// rule_registry.go (rule); capabilities in capability_loader.go and
// actions/catalog.go; the underscore rule in core/dslfs/walker.go; the
// authored lifecycle in authoring_session.go, authoring_staged.go,
// authoring_promote_durable.go and authoring_sandbox.go.
const docDisabled = "Takes the construct out of service without deleting it. Every construct is enabled by default and `@enabled` " +
	"is an accepted no-op, so `@disabled` is the only off switch. A disabled construct stays in the tree, is still maintained, " +
	"and must stay valid, because re-enabling it is only removing the annotation. What it does depends on the construct:\n" +
	"\n" +
	"- A disabled query, mutation, logic or builtin is hidden from `functions()` and from the MCP surface, its `@mcp` tool " +
	"included, and a direct call is refused with `function \"name\" is disabled`; `help()` still describes it, reporting " +
	"`\"enabled\": false`. A disabled builtin also skips the check that its Go executor exists, so a builtin whose executor " +
	"is gone can be disabled without refusing boot.\n" +
	"- A disabled automation is neither scheduled nor subscribed to its trigger, is dropped from the MCP surface, and is " +
	"refused on every manual run path: the MCP dry run and live run, the run relay and the HTTP trigger.\n" +
	"- A disabled tool, seed, prompt, provider, action, spec or trait is skipped at load. A tool is not registered, and its " +
	"name stays reserved so no ungoverned tool is generated from the function behind it. A seed is never materialised and a " +
	"prompt never registers. A provider is not registered and no auth is resolved for it, and on a `@base` provider it skips " +
	"every provider that `@extends` it. An action is parsed but not registered. A spec or trait is still validated but never " +
	"binds, and its name stays reserved, so a call to it says it is disabled rather than not found.\n" +
	"- A disabled rule is loaded but never evaluated.\n" +
	"- A disabled capability is still reconciled against its Go capability and checked for duplicates -- `@disabled` is not " +
	"a validation bypass -- but it is left out of the catalog: `use capabilities....` imports stop resolving it, and an action " +
	"that names it is refused at load with `capability \"name\" is @disabled`. To park DSL ahead of the Go it needs, put the " +
	"file under a `_disabled/` directory instead, which the loaders skip.\n" +
	"\n" +
	"An authored spec or trait keeps the same state through the authoring lifecycle. Staging or promoting a disabled one is " +
	"refused (`construct is @disabled; enable it (remove @disabled from the source) before promoting`). A promoted row that " +
	"is disabled is skipped when the engine re-hydrates authored constructs and counted as `skippedDisabled` rather than " +
	"quarantined; it reserves the name only when nothing live or already reserved holds it, and promoting the corrected " +
	"source or demoting the name releases it. A staged one is skipped without reserving anything. A bundle that authors a " +
	"disabled capability is refused (`capability \"name\" is @disabled`), because a disabled capability compiles to nothing."

// docAddToSet and docRemoveFromSet are the one-paragraph Docs entries (hover
// reads them); the mutation placements add docSetMembershipRules for the page.
const docAddToSet = "On an update mutation: treat the named array-typed payload fields as SETS and UNION the written elements " +
	"into the stored array -- deduped, existing order kept, new members appended in the order given. The membership half " +
	"@appendFields is not: append is not deduped and has no counterpart that removes, so a toggle built on it duplicates on a " +
	"double click. Pairs with @removeFromSet. Format: @addToSet(\"disabledDeployables\"). See memql#4951."

const docRemoveFromSet = "On an update mutation: treat the named array-typed payload fields as SETS and REMOVE the written " +
	"elements from the stored array, keeping the order of what remains. Removing something absent is a no-op rather than an " +
	"error, so the mutation is idempotent and two callers removing the same member both succeed. Pairs with @addToSet, and " +
	"the pair is what lets a set be edited one member at a time instead of read-modify-written whole. Format: " +
	"@removeFromSet(\"disabledDeployables\"). See memql#4951."

// docSetMembershipRules is what @addToSet and @removeFromSet share. The rules
// are setMembershipFields and memberKey in component/memql/executor_mutation.go
// and mutationSetFields and validateSetMembershipFields in
// component/memql/mutation_templates.go.
const docSetMembershipRules = "Both follow the same rules:\n" +
	"\n" +
	"- Only an update mutation may carry it; on an insert it is refused at load, because an insert has no stored set to " +
	"change.\n" +
	"- A field absent from the write is untouched: the stored array survives the read-merge.\n" +
	"- Members compare by their rendered form, so `1` and `\"1\"` are one member. A stored number comes back from JSON as a " +
	"float and its string spelling as a string, and keeping both would be a set that quietly grew.\n" +
	"- A stored value that is not an array reads as empty, and a scalar written where an array is expected is one member, as " +
	"with `@appendFields`.\n" +
	"- A field named by two of `@appendFields`, `@addToSet` and `@removeFromSet` is refused at load: each rewrites the same " +
	"key, so which one won would depend on the executor's order rather than on anything the mutation says."

// docNoUnset is the Docs entry; the mutation placement adds what "empty"
// means, which is isEmptyPayloadValue in component/memql/executor_mutation.go.
const docNoUnset = "On any mutation: declare the named payload fields ONE-WAY -- a write may set them or change one non-empty " +
	"value to another, but may never take a stored non-empty value back to empty. On the read-merge path a named field " +
	"arriving empty is dropped from the delta when the stored row holds a non-empty value. Closes the gap read-merge cannot " +
	"(it only inherits fields ABSENT from a delta, so a body writing `f: args.f ?? \"\"` blanks the stored value with an " +
	"explicit empty string). Distinct from @createOnly, which forbids any post-create write; @noUnset forbids only set -> " +
	"unset, so a legitimately-later stamp still lands. Format: @noUnset(\"bootstrappedAt\"). See memql#3415."

const docNoUnsetEmpty = "Empty means nil, a blank or whitespace-only string, or an empty array or object. A numeric or " +
	"boolean zero is a value, not an unset: `0` and `false` are written like any other value, and treating them as empty " +
	"would make `@noUnset` unwritable for those types."

// docDefaultConceptField is @default on a concept field. The lowering is
// parseTypedDefaultValue in component/database/memory-nodes/concept_parser.go
// (pinned by TestDefaultIsLoweredByItsDeclaredType, TestBadDefaultRefusesToLoad
// and TestBareDefaultLiteralIsRead); the gate is test/dslconformance's
// TestDefaultIsCoalescedOrStamped, whose scope dsl/_reference/_concept.memql
// section 8 states.
const docDefaultConceptField = "The default the concept schema declares for the field. Declared metadata: it is emitted into " +
	"the schema and NEVER applied on insert -- `??` in the mutation is what fills a value (memql#2960). The emitted `default` " +
	"is still read by the SDK, editor hover and form generators, so it has to be right.\n" +
	"\n" +
	"The literal is lowered against the field's declared type, and one that could never be a value of that type is refused " +
	"at load (memql#3248):\n" +
	"\n" +
	"- `bool`: exactly `true` or `false`.\n" +
	"- `int`: a base-10 integer.\n" +
	"- `float`: a number; an integer literal is a valid float.\n" +
	"- `datetime`: an RFC3339 timestamp, or `\"\"` for unset.\n" +
	"- `string` and `enum`: the literal verbatim, never coerced, so `@default(\"0\")` on a string field is the string `\"0\"`.\n" +
	"- `object`, `array`, `map` and `any`: an untyped lowering, because the declaration does not narrow the literal to one " +
	"reading.\n" +
	"\n" +
	"Bare and quoted spellings are equivalent: `@default(false)` and `@default(\"false\")` both declare the bool `false`, and " +
	"`@default(7)` declares the number 7.\n" +
	"\n" +
	"A default nothing stamps is caught at authoring time (memql#3038): `TestDefaultIsCoalescedOrStamped` fails when an " +
	"optional, top-level concept field carries `@default` and no mutation bound to the concept stamps it. Only a stamped " +
	"value counts -- `f: args.f ?? \"v\"`, a literal, or a computed expression; `accept { f }`, a bare `args.f` shorthand and a " +
	"plain `f: args.f` all bind the field to a caller argument, so omitting the argument still writes nothing. Two things are " +
	"outside the gate: a domain mounted at runtime through `MEMQL_DSL_PATH`, which it never scans, and a `@default` on a leaf " +
	"inside an object block, which no write form can stamp because a mutation writes the parent object whole."

// docProviderType is @type on a provider: newAIProvider in
// component/memql/ai_providers.go (matched case-insensitively), whose error
// leaves the provider registered but unavailable (unified_kinds_loader.go), and
// the streaming parameter of epic memql#5137.
const docProviderType = "The provider's type, which picks the client that serves it: `OpenAI` (or `OpenAIChat`), " +
	"`OpenAITTS` or `OpenAIEmbedding` for OpenAI, and `Anthropic` (or `AnthropicChat`) for Anthropic, matched without regard " +
	"to case. `Fleet` and `SubscriptionApp` are accepted on a `@base` provider only: their models are named from a policy " +
	"(`fleet:<model>`, `app:<id>`) rather than declared as children. Any other type leaves the provider registered but " +
	"unavailable (`unsupported provider type`). Streaming is a parameter (`streaming true` in `params`), not a type, and a " +
	"child that `@extends` a base takes the base's type."

// docVersionConcept is @version on a concept; the seed placement carries its
// own.
const docVersionConcept = "Version tag for the concept: a semver string, @version(\"1.0.0\"). Metadata only -- canonical ids " +
	"are not versioned by it (#2613)."

// docRowAuthz is the Docs entry for @rowAuthz (hover reads it as well as the
// page). component/memql's TestRowAuthzDocDoesNotClaimEnforcementIsInert and
// TestRowAuthzDocCitesTestsThatExist hold it: it must say it is ENFORCED ON THE
// READ PATH (memql#3172) and cite the five tests by name.
const docRowAuthz = "Declares WHO MAY SEE this concept's rows, once on the concept, instead of as an `actor.*` term every " +
	"filter over it must remember to carry. Four tiers, one spelling each: @rowAuthz(public) (globally readable by intent -- " +
	"spelled explicitly, because \"no annotation\" and \"declared public\" are different states), @rowAuthz(clusterOwner) " +
	"(administrative), @rowAuthz(owner=\"<field>\") (the field is compared against actor.userId; it must be a field the concept " +
	"declares, OR the literal \"id\" for a SELF-OWNED concept whose owner is the row itself -- memql#3029; `id` and only `id`, " +
	"since createdBy means who WROTE the row, not whose row it is), and @rowAuthz(via=\"<spec>\") (a relationship spec grants " +
	"visibility).\n" +
	"\n" +
	"A fifth FORM, not a fifth tier: @rowAuthz(owner=\"<field>\", clusterOwner) is the composite -- the owner, OR a cluster " +
	"owner (memql#4312), in either order -- and the form an operator console needs over per-user rows, since a plain owner= " +
	"tier has no cluster-owner bypass. The other keys (account=, rankVisible, rankStrict, unowned=, requiresIdentity, " +
	"rankFloor=) qualify a tier rather than naming one.\n" +
	"\n" +
	"ENFORCED ON THE READ PATH since Phase 3 (memql#3172): declaring a tier CHANGES WHAT READS RETURN. Two mechanisms, and " +
	"neither consults the filter to decide whether to engage -- the tier's predicate is ANDed into the plan before the read " +
	"runs (resolved from the construct's declared binding, so the narrowing pushes down into SQL), and every row leaving the " +
	"engine is separately admitted against the tier ITS OWN concept declares, which is the only mechanism available to a raw " +
	"client-supplied query string, to graph expansion, or to a TOP-LEVEL BUILTIN CALL whose rows come out of a Go handler " +
	"(memql#3982) -- none of which has a filter to AND anything into.\n" +
	"\n" +
	"SUBSCRIPTIONS are gated by the same row admission (memql#4309): a graph.node.* event reaches a stream only if the tier " +
	"admits the row for that stream's actor, a `granted` row arrives id-only with payload_omitted set for the client to " +
	"re-read, and an UNDECLARED concept is delivered to everyone exactly as its reads already return to everyone -- the live " +
	"feed mirrors the read path rather than running a second rulebook. The write side is enforced too: update/delete refuse " +
	"when the target row's declared owner is not the actor (memql#3174).\n" +
	"\n" +
	"Implementation: component/memql/rowauthz_enforce.go, called from parser.go. MEASURED BY " +
	"TestClusterOwnerTierInjectsTheAdminGate, TestFilteredReadPathAppliesTheRowGate, " +
	"TestGraphExpansionAppliesTheTraversalGateBeforeItEmitsTheRow, TestTopLevelBuiltinAppliesTheRowGate and " +
	"TestSubscriptionFanOutAppliesTheRowGate -- named so a reader can check whether this is still true rather than trust the " +
	"sentence. Trusting it would have been wrong before: this doc described the tier as parsed-but-unread for as long as " +
	"Phase 3 had been live, which is false in the one direction that costs a reader a wrong authorization assumption, and " +
	"because the doc also feeds editor hover, the error reached every author who hovered the annotation (memql#3727). See " +
	"docs/public/operate/auth/per-row-authz-audit.md and memql#2803."

// docSecretField is @secret on a concept field: the scope statement memql#3036
// and #3182-#3184 settled. component/database/memory-nodes'
// TestSecretEnforcementIsRealAndScoped reads the generated page and fails when
// a surface goes unnamed, or when a surface that now redacts is still described
// as uncovered. Extend the covered list by re-running the enumeration it
// describes, never by appending to it.
const docSecretField = "The field holds a secret. It is emitted as `x-secret`, and every validation surface that quotes a rejected value " +
	"replaces the value with `<redacted>` while the argument name and the declared constraint (enum members, bounds, pattern) " +
	"survive, so the diagnostic stays usable.\n" +
	"\n" +
	"The covered surfaces come from an exhaustive enumeration of validators -- every jsonschema `.Validate(` call site and every " +
	"value-quoting rejection message in the engine -- not from adding one entry at a time. Three incremental passes each shipped a " +
	"scope statement that walked past a surface the next pass found, so extend the list by re-running the enumeration, never by " +
	"appending to it.\n" +
	"\n" +
	"- The **function-args validator** (`component/memql/function_validator.go`): enum, minimum, maximum, pattern and date-time " +
	"(memql#3036).\n" +
	"- The **tool-args validator** (`MemQLEngine.validateToolArgs`, `component/memql/tool_execution.go`), compiled from the same " +
	"args schema and running before the function-args validator on the agent path, so it is the surface a rejected secret reaches " +
	"first. Both of its exits redact (memql#3182): the message returned to the model, and the WARN, which redacts per key from the " +
	"args schema instead of serializing the whole args map, and falls back to a values-free `argKeys` list when the tool's " +
	"arguments cannot be classified.\n" +
	"- The **automation args binder** (`component/automations/args_binding.go`), which applies the same rules to event payloads: a " +
	"`graph.node.created` event carries the concept row's fields flattened into its payload. The loader stamps the secret flag " +
	"from the trigger topic's concept, so the enum and pattern refusals, and the WARN log they are written to, all print " +
	"`<redacted>`, closing the one path by which a row value could reach a structured log (memql#3183).\n" +
	"- **Concept payload validation** (`Concept.validate`, so `Create` and `Delete`), where `@minimum`, `@maximum` and `@format` " +
	"declared on the concept are enforced. The six jsonschema keywords that interpolate the instance value (`minimum`, `maximum`, " +
	"`exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`, `format`) all redact, and every other message stays byte-identical to " +
	"an unannotated field's (memql#3184).\n" +
	"- The DSL-callable `validate` and `preflight` builtins (`component/memql/executor_builtin.go`), which compile the same concept " +
	"schema and return every leaf message to the caller in a result payload.\n" +
	"\n" +
	"The last two resolve secrecy through the schema at the failing instance location, so they cover `@secret` at any nesting " +
	"depth and inherit it onto array elements, unlike `Concept.SecretFields()`, which is top-level only and is not the accessor " +
	"those paths use.\n" +
	"\n" +
	"On the two args-validator surfaces, **matching is by argument NAME, not by write target**: an args field is redacted when its " +
	"name appears among the bound concept's `@secret` fields. A mutation writing `apiKey: args.credential` into a `@secret` " +
	"`apiKey` leaves `credential` unredacted, and renaming between argument and field is common, so do not rely on the write " +
	"target. The concept-payload and builtin surfaces resolve through the schema and are not subject to this rule.\n" +
	"\n" +
	"A `@secret` value is **not** redacted from **query results**: any query that projects it returns it in full. That is an " +
	"authorization decision, which needs a definition of \"elevated\" and depends on the per-row authorization model (memql#2803).\n" +
	"\n" +
	"**Length is never redacted anywhere**, deliberately and uniformly: `value too long (N runes, max M)` and the jsonschema " +
	"`minLength` and `maxLength` messages report a rune count for a secret field too. They quote no value, but a length is a " +
	"disclosure; redacting it on one surface would make that surface the only one that differs while withholding nothing the " +
	"others already print.\n" +
	"\n" +
	"Prompt input-schema validation (`PromptTemplate.ValidateData`, `component/memql/ai_prompts.go`) is unclassified: it runs a " +
	"jsonschema over caller data and interpolates the instance the same way, but its schema is built from the prompt's own field " +
	"list rather than from a concept, so there is no `x-secret` in it to read. Nothing is redacted there; treat it as uncovered, " +
	"not as safe.\n" +
	"\n" +
	"So `@secret` stops a credential leaking through a validation diagnostic. It is not a general secrecy guarantee, and not a " +
	"reason to treat a credential in the graph as protected."
