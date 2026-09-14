// Package annotations is the one registry of the MemQL annotation surface:
// every place an annotation can be written (a Receiver), every annotation
// each place accepts (a Placement), the argument forms and keyword keys it
// takes, an example of it, and the check every parser runs (Check, CheckAll).
//
// It is a leaf package -- it imports nothing inside the repo -- so the
// parser, the concept translator in component/database, the editor surface
// (component/memql/sense) and the DSL spec (component/language/dslspec) all
// derive from it without an import cycle.
//
// # One gate
//
// Every construct parser and every field list in component/language/parser
// converts the annotations it read into Uses (parser.AnnotationUse) and calls
// CheckAll at PARSE time; the concept translator does the same for a concept,
// its body (@relationship) and its fields. The per-construct parsers keep the
// argument SEMANTICS (what a value means, whether it is well-formed); which
// names, forms and keys are legal is decided here and nowhere else. Before
// this registry the answer lived in eleven places -- a load-time text scan,
// inline switches in the tool / provider / seed / action parsers, duplicate
// lists in four converters and the concept loader's own switch -- and they
// disagreed (memql#5359).
package annotations

import "sort"

// ArgSpec is one keyword key an annotation accepts inside @name(...). A bare
// flag key -- `clusterOwner` in `@rowAuthz(owner="x", clusterOwner)` -- has
// Type "flag".
type ArgSpec struct {
	Name string // keyword-arg name, e.g. "event"
	Type string // "string", "int", "flag", ...
	Doc  string // one-line completion/hover doc
}

// Placement is one annotation on one receiver: the argument forms it takes,
// its keyword keys, whether it may be written more than once, the canonical
// example, and a receiver-specific doc when the name means something
// different here than elsewhere.
type Placement struct {
	Receiver   Receiver
	Name       string
	Forms      Form
	Keys       []ArgSpec // closed key set for FormKeywords; a bare flag key has Type "flag"
	Repeatable bool
	Example    string // canonical use on this receiver, e.g. `@cache(300)`
	Doc        string // receiver-specific doc; empty falls back to Docs[Name]
}

// Placements returns every placement, in receiver order (Receivers) and then
// by name. The result is a fresh slice the caller may keep.
func Placements() []Placement {
	return append([]Placement(nil), placements...)
}

// Lookup returns the placement of name on r, and whether r accepts name.
func Lookup(r Receiver, name string) (Placement, bool) {
	p, ok := placementIndex[r][name]
	return p, ok
}

// ByReceiver maps each receiver's name to the annotation names it accepts,
// sorted. It is DERIVED from the placements and kept for the consumers that
// only need names (completion, the dslspec projection); the check reads the
// placements themselves. The concept's key is "Concept" (it was "" before
// memql#5359).
var ByReceiver = func() map[string][]string {
	out := map[string][]string{}
	for _, p := range placements {
		out[string(p.Receiver)] = append(out[string(p.Receiver)], p.Name)
	}
	return out
}()

// KeywordArgs maps an annotation name to the keyword keys its parenthesised
// form accepts. It is the editor's view (completion inside @name(...), hover
// on a key), DERIVED from the placements: for a name whose keyword form sits
// on more than one receiver, the first placement in receiver order speaks for
// it.
var KeywordArgs = func() map[string][]ArgSpec {
	out := map[string][]ArgSpec{}
	for _, p := range placements {
		if len(p.Keys) == 0 {
			continue
		}
		if _, seen := out[p.Name]; !seen {
			out[p.Name] = p.Keys
		}
	}
	return out
}()

// KeywordArgsFor returns the keyword keys an annotation accepts inside its
// @name(...) form, or nil when it takes none.
func KeywordArgsFor(name string) []ArgSpec {
	return KeywordArgs[name]
}

// docFor returns the doc a placement shows: its own, or the name's.
func docFor(p Placement) string {
	if p.Doc != "" {
		return p.Doc
	}
	return Docs[p.Name]
}

// placementsNamed returns every placement of name, in registry order.
func placementsNamed(name string) []Placement {
	var out []Placement
	for _, p := range placements {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// placements is the registry, sorted into Placements order at init.
var placements = func() []Placement {
	out := append([]Placement(nil), placementTable...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := receiverIndex[out[i].Receiver], receiverIndex[out[j].Receiver]
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}()

// placementIndex is receiver -> name -> placement.
var placementIndex = func() map[Receiver]map[string]Placement {
	out := map[Receiver]map[string]Placement{}
	for _, p := range placements {
		if out[p.Receiver] == nil {
			out[p.Receiver] = map[string]Placement{}
		}
		out[p.Receiver][p.Name] = p
	}
	return out
}()

// lifecycle returns the three annotations almost every construct takes:
// @description, @enabled, @disabled. Every @disabled placement carries the
// same docDisabled, which says what disabling does to each kind of construct.
func lifecycle(r Receiver, description string) []Placement {
	return []Placement{
		{Receiver: r, Name: "description", Forms: FormString, Example: `@description("` + description + `")`},
		{Receiver: r, Name: "enabled", Forms: FormFlag, Example: "@enabled"},
		{Receiver: r, Name: "disabled", Forms: FormFlag, Example: "@disabled", Doc: docDisabled},
	}
}

// Receiver-specific docs, for the names that mean something different on
// different receivers.
const (
	docActorOnFunction = "Declares that the body reads the authenticated actor (actor.userId, actor.role, actor.identityId, actor.isClusterOwner, actor.primaryEmail, actor.now). The actor-binding load rule refuses a body that reads actor.* without it (memql#2621)."
	docActorOnShape    = "Shape kind marker: the shape projects the authenticated actor's envelope (actor.userId / actor.role / ...), and carries no signature concept."
)

// Keyword key sets, shared by the placement and its docs.
var (
	triggerKeys = []ArgSpec{
		{Name: "event", Type: "string", Doc: "Event pattern, e.g. \"node.created\" (with concept=) or a raw topic such as \"system.startup\"."},
		{Name: "concept", Type: "string", Doc: "Concept id the triggering event targets; required by the structured node.* event kinds."},
		{Name: "partition", Type: "string", Doc: "Partition selector, e.g. \"*\" for all partitions. Required while the event topic carries a partition segment (#56 phase 8)."},
		{Name: "schedule", Type: "string", Doc: "Cron schedule with a leading seconds field, e.g. \"0 0 * * * *\"."},
		{Name: "filter", Type: "string", Doc: "The trigger filter as a keyword; the standalone @filter(...) annotation is the usual spelling and sets the same filter."},
		{Name: "on", Type: "string", Doc: "A synonym for event=: on=<concept>.<created|updated|deleted>, with the concept named through the file's `use` import, folds to the same graph.node.<action>.<concept> pattern event= names (resolved by the automation loader and the concept resolver). A later epic retires the synonyms (D15/D17)."},
	}
	scheduleKeys = []ArgSpec{
		{Name: "cron", Type: "string", Doc: "Cron schedule, e.g. \"0 0 * * * *\". Synonym for @trigger(schedule=...)."},
	}
	handlerKeys = []ArgSpec{
		{Name: "type", Type: "string", Doc: "Handler type: \"function\", \"query\", \"webhook\" or \"delegate\". Required."},
		{Name: "name", Type: "string", Doc: "Function or builtin name (with type=\"function\")."},
		{Name: "query", Type: "string", Doc: "MemQL query or mutation call (with type=\"query\")."},
		{Name: "url", Type: "string", Doc: "Webhook URL (with type=\"webhook\")."},
		{Name: "method", Type: "string", Doc: "HTTP method for a webhook handler, e.g. \"POST\"."},
	}
	rateLimitKeys = []ArgSpec{
		{Name: "maxCalls", Type: "int", Doc: "Maximum calls allowed per period."},
		{Name: "periodSeconds", Type: "int", Doc: "Rate-limit window in seconds."},
	}
	cacheKeys = []ArgSpec{
		{Name: "ttl", Type: "string", Doc: "Cache TTL in whole seconds. Positional preferred (#2618): @cache(300); keyword ttl=\"300\" keeps parsing."},
	}
	whenKeys = []ArgSpec{
		{Name: "level", Type: "string", Doc: "The level the call declared: fast, strong, reasoning or embeddings."},
		{Name: "modality", Type: "string", Doc: "The modality derived from the call site: chat, streamingChat, tools, streamingTools, structured, vision, embedding, speech, transcribe."},
		{Name: "prompt", Type: "string", Doc: "The DSL prompt this call renders. Empty matches a Go call site with no prompt."},
		{Name: "role", Type: "string", Doc: "The AGENT's role slug. Distinct from actorRole: this is what is acting."},
		{Name: "actorRole", Type: "string", Doc: "The calling human's cluster role. Distinct from role: this is who is watching."},
		{Name: "tag", Type: "string", Doc: "A call tag, e.g. \"background\" or \"backgroundEscalation\"."},
		{Name: "touches", Type: "string", Doc: "A concept id PREFIX the call's footprint matches (startsWith semantics)."},
	}
	builtinArgsKeys = []ArgSpec{
		{Name: "profile", Type: "string", Doc: "The argument profile: none, object, optionalObject, stringOrObject, optionalString or optionalStringOrObject. Inferred from the body when absent."},
		{Name: "stringKey", Type: "string", Doc: "The key a bare string argument binds to, for the profiles that accept one."},
		{Name: "additionalProperties", Type: "string", Doc: "Whether an object argument may carry keys the body does not declare (default false)."},
		{Name: "when", Type: "string", Doc: "An inert note naming the type of routingRuleActivate's `when` argument, which its body cannot declare (`when` is a keyword). No loader reads it."},
		{Name: "excludes", Type: "string", Doc: "An inert note naming the type of routingRuleActivate's `excludes` argument. No loader reads it."},
	}
	relationshipKeys = []ArgSpec{
		{Name: "type", Type: "string", Doc: "STRUCTURAL type -- what the engine does with the edge. Closed set: parent, owns, createdBy, alias, equals, contains, references."},
		{Name: "field", Type: "string", Doc: "Local field holding the foreign key; a dotted path reaches into a nested object block."},
		{Name: "fieldSource", Type: "string", Doc: "Where the foreign key lives: \"payload\" (the default) or \"table\" (the edge table)."},
		{Name: "target", Type: "string", Doc: "Target concept: a bare concept name resolved through a file-top `use` import."},
		{Name: "direction", Type: "string", Doc: "\"outgoing\" or \"incoming\"."},
		{Name: "as", Type: "string", Doc: "DOMAIN label -- what the edge means, e.g. as=\"assignedTo\". Optional. Any lowerCamelCase identifier; validated for form only and never checked against a list, so a new verb never needs an engine release (memql#3652)."},
	}
	rowAuthzKeys = []ArgSpec{
		{Name: "public", Type: "flag", Doc: "The public tier: globally readable by intent."},
		{Name: "clusterOwner", Type: "flag", Doc: "The administrative tier; beside owner=, the composite -- the owner, or a cluster owner (memql#4312)."},
		{Name: "owner", Type: "string", Doc: "The owned tier: the payload field compared against actor.userId, or \"id\" for a self-owned concept (memql#3029)."},
		{Name: "via", Type: "string", Doc: "The granted tier: the relationship spec that grants visibility."},
		{Name: "account", Type: "string", Doc: "Beside owner=: also admits anyone whose group ties them to the row's account field (epic memql#5165)."},
		{Name: "rankVisible", Type: "flag", Doc: "Beside owner=: reads widen to anyone at or above the owner's rank (epic memql#4832)."},
		{Name: "rankStrict", Type: "flag", Doc: "Beside owner= and rankVisible: writes widen to rows owned strictly below the caller's rank, and the cluster-owner write escape is withdrawn."},
		{Name: "unowned", Type: "string", Doc: "Beside owner= and rankVisible: the actor rank from which a row with an empty owner is readable."},
		{Name: "requiresIdentity", Type: "flag", Doc: "Beside public: readable by authenticated callers only (memql#4809)."},
		{Name: "rankFloor", Type: "string", Doc: "Beside clusterOwner: relaxes the READ to a rank floor while the write stays cluster-owner (memql#5216)."},
	}
	displayCardKeys = []ArgSpec{
		{Name: "primary", Type: "string", Doc: "The field shown as the row's title. Required."},
		{Name: "secondary", Type: "string", Doc: "The field shown under the title."},
		{Name: "tertiary", Type: "string", Doc: "A third field shown in the row's detail line."},
		{Name: "status", Type: "string", Doc: "The field shown as the row's status."},
	}
	composableKeys = []ArgSpec{
		{Name: "as", Type: "string", Doc: "The label a composed file names the row kind by, e.g. \"invoice\"."},
		{Name: "fields", Type: "string", Doc: "Comma-separated fields to compose from, e.g. \"number,issuedAt,total\"; each must be a field the concept declares or a row intrinsic."},
		{Name: "list", Type: "string", Doc: "The bare name of the query that lists the rows to compose from."},
	}
	variantKeys = []ArgSpec{
		{Name: "discriminator", Type: "string", Doc: "The field whose value picks the branch."},
	}
)

// placementTable is the registry, receiver by receiver.
var placementTable = concat(
	// ---- Query ----------------------------------------------------------
	lifecycle(Query, "List the caller's open tickets, newest first."),
	[]Placement{
		{Receiver: Query, Name: "actor", Forms: FormFlag, Example: "@actor", Doc: docActorOnFunction},
		{Receiver: Query, Name: "cache", Forms: FormNumber | FormKeywords, Keys: cacheKeys, Example: "@cache(300)"},
		{Receiver: Query, Name: "latestMode", Forms: FormFlag, Example: "@latestMode"},
		{Receiver: Query, Name: "mcp", Forms: FormFlag, Example: "@mcp"},
		{Receiver: Query, Name: "nocache", Forms: FormFlag, Example: "@nocache"},
		{Receiver: Query, Name: "public", Forms: FormFlag, Example: "@public"},
		{Receiver: Query, Name: "requiresCapability", Forms: FormString | FormStrings, Example: `@requiresCapability("read", "principal")`},
		{Receiver: Query, Name: "requiresRank", Forms: FormString, Example: `@requiresRank("developer")`},
		{Receiver: Query, Name: "serverOnly", Forms: FormFlag, Example: "@serverOnly"},
		{Receiver: Query, Name: "unbounded", Forms: FormString, Example: `@unbounded("a small catalog of fixed size")`},
	},

	// ---- Mutation -------------------------------------------------------
	lifecycle(Mutation, "Rename one of the caller's tickets."),
	[]Placement{
		{Receiver: Mutation, Name: "actor", Forms: FormFlag, Example: "@actor", Doc: docActorOnFunction},
		{Receiver: Mutation, Name: "addToSet", Forms: FormString | FormStrings, Example: `@addToSet("disabledDeployables")`, Doc: docAddToSet + "\n\n" + docSetMembershipRules},
		{Receiver: Mutation, Name: "appendFields", Forms: FormString | FormStrings, Example: `@appendFields("attachmentIds")`},
		{Receiver: Mutation, Name: "createOnly", Forms: FormString | FormStrings, Example: `@createOnly("status", "attempts")`},
		{Receiver: Mutation, Name: "mcp", Forms: FormFlag, Example: "@mcp"},
		{Receiver: Mutation, Name: "mergeFields", Forms: FormString | FormStrings, Example: `@mergeFields("preferences")`},
		{Receiver: Mutation, Name: "noUnset", Forms: FormString | FormStrings, Example: `@noUnset("bootstrappedAt")`, Doc: docNoUnset + "\n\n" + docNoUnsetEmpty},
		{Receiver: Mutation, Name: "public", Forms: FormFlag, Example: "@public"},
		{Receiver: Mutation, Name: "removeFromSet", Forms: FormString | FormStrings, Example: `@removeFromSet("disabledDeployables")`, Doc: docRemoveFromSet + "\n\n" + docSetMembershipRules},
		{Receiver: Mutation, Name: "requiresCapability", Forms: FormString | FormStrings, Example: `@requiresCapability("execute", "app:deployables/publish")`},
		{Receiver: Mutation, Name: "requiresRank", Forms: FormString, Example: `@requiresRank("admin")`},
		{Receiver: Mutation, Name: "scrubPii", Forms: FormFlag, Example: "@scrubPii"},
		{Receiver: Mutation, Name: "serverOnly", Forms: FormFlag, Example: "@serverOnly"},
	},

	// ---- Logic ----------------------------------------------------------
	lifecycle(Logic, "Close every ticket that has been idle for a week."),
	[]Placement{
		{Receiver: Logic, Name: "actor", Forms: FormFlag, Example: "@actor", Doc: docActorOnFunction},
		{Receiver: Logic, Name: "eventField", Forms: FormString | FormStrings, Example: `@eventField("id", "status")`},
		{Receiver: Logic, Name: "requiresCapability", Forms: FormString | FormStrings, Example: `@requiresCapability("execute", "app:deployables/deploy")`},
		{Receiver: Logic, Name: "requiresRank", Forms: FormString, Example: `@requiresRank("admin")`},
	},

	// ---- Automation -----------------------------------------------------
	lifecycle(Automation, "On a new ticket, notify its owner."),
	[]Placement{
		{Receiver: Automation, Name: "actor", Forms: FormFlag, Example: "@actor", Doc: docActorOnFunction},
		{Receiver: Automation, Name: "filter", Forms: FormExpression | FormString, Example: `@filter(payload.status == "open")`},
		{Receiver: Automation, Name: "mcp", Forms: FormFlag, Example: "@mcp"},
		{Receiver: Automation, Name: "schedule", Forms: FormKeywords | FormString, Keys: scheduleKeys, Example: `@schedule(cron="0 0 * * * *")`},
		{Receiver: Automation, Name: "template", Forms: FormFlag, Example: "@template"},
		{Receiver: Automation, Name: "trigger", Forms: FormKeywords, Keys: triggerKeys, Example: `@trigger(event="node.created", concept="v1:cluster:node")`},
	},

	// ---- Action / Capability --------------------------------------------
	lifecycle(Action, "Tag a release in the repository."),
	lifecycle(Capability, "Create a git tag and a GitHub release for a version."),
	[]Placement{
		{Receiver: Capability, Name: "sideEffect", Forms: FormString, Example: `@sideEffect("write")`},
	},

	// ---- Spec (spec and trait) --------------------------------------------
	lifecycle(Spec, "Matches the rows that are still open."),

	// ---- Tool -----------------------------------------------------------
	lifecycle(Tool, "Create a to-do for the caller."),
	[]Placement{
		{Receiver: Tool, Name: "allowedRoles", Forms: FormString | FormStrings, Example: `@allowedRoles("assistant", "specialist")`},
		{Receiver: Tool, Name: "destructive", Forms: FormFlag, Example: "@destructive"},
		{Receiver: Tool, Name: "executionTime", Forms: FormString, Example: `@executionTime("fast")`},
		{Receiver: Tool, Name: "handler", Forms: FormKeywords, Keys: handlerKeys, Example: `@handler(type="function", name="createTodo")`},
		{Receiver: Tool, Name: "mcp", Forms: FormFlag, Example: "@mcp"},
		{Receiver: Tool, Name: "rateLimit", Forms: FormKeywords, Keys: rateLimitKeys, Example: `@rateLimit(maxCalls=10, periodSeconds=60)`},
		{Receiver: Tool, Name: "requiresConfirmation", Forms: FormFlag, Example: "@requiresConfirmation"},
		{Receiver: Tool, Name: "scopes", Forms: FormString | FormStrings, Example: `@scopes("operator")`},
	},

	// ---- Builtin --------------------------------------------------------
	lifecycle(Builtin, "Preflight and start a campaign send."),
	[]Placement{
		{Receiver: Builtin, Name: "alias", Forms: FormString, Repeatable: true, Example: `@alias("memqlVersion")`},
		{Receiver: Builtin, Name: "args", Forms: FormKeywords, Keys: builtinArgsKeys, Example: `@args(profile="object")`},
		{Receiver: Builtin, Name: "executor", Forms: FormString, Example: `@executor("integration.campaigns.startSend")`},
		{Receiver: Builtin, Name: "requiresCapability", Forms: FormString | FormStrings, Example: `@requiresCapability("execute", "app:deployables/deploy")`},
		{Receiver: Builtin, Name: "sdk", Forms: FormFlag, Example: "@sdk"},
	},

	// ---- Prompt ---------------------------------------------------------
	lifecycle(Prompt, "Distil a cluster of episodes into one memory."),
	[]Placement{
		{Receiver: Prompt, Name: "defaultProvider", Forms: FormString, Example: `@defaultProvider("fleet")`},
		{Receiver: Prompt, Name: "level", Forms: FormString, Example: `@level("fast")`, Doc: "How much intelligence the call needs: fast, strong, reasoning or embeddings. Required on every prompt; the router's rules branch on it, so a prompt never names a model (epic memql#5127)."},
		{Receiver: Prompt, Name: "templateFile", Forms: FormString, Example: `@templateFile("prompts/consolidateMemory.tmpl")`, Doc: "The prompt's template: a .tmpl file beside the prompt, rendered with the input fields."},
	},

	// ---- Provider -------------------------------------------------------
	lifecycle(Provider, "OpenAI GPT-5.4 Mini -- balanced cost and latency."),
	[]Placement{
		{Receiver: Provider, Name: "base", Forms: FormFlag, Example: "@base"},
		{Receiver: Provider, Name: "default", Forms: FormFlag, Example: "@default", Doc: "Mark this provider as the default for its modality."},
		{Receiver: Provider, Name: "extends", Forms: FormString, Example: `@extends("openai")`},
		{Receiver: Provider, Name: "modality", Forms: FormString, Example: `@modality("embedding")`},
		{Receiver: Provider, Name: "model", Forms: FormString, Example: `@model("gpt-5.4-mini")`},
		{Receiver: Provider, Name: "type", Forms: FormString, Example: `@type("OpenAI")`, Doc: docProviderType},
	},

	// ---- Shape ----------------------------------------------------------
	[]Placement{
		{Receiver: Shape, Name: "actor", Forms: FormFlag, Example: "@actor", Doc: docActorOnShape},
		{Receiver: Shape, Name: "description", Forms: FormString, Example: `@description("Ticket summary card.")`},
		{Receiver: Shape, Name: "row", Forms: FormFlag, Example: "@row"},
	},

	// ---- Policy ---------------------------------------------------------
	[]Placement{
		{Receiver: Policy, Name: "description", Forms: FormString, Example: `@description("Local strongest, then an app, then the cheapest federated model.")`},
		{Receiver: Policy, Name: "fallback", Forms: FormString, Repeatable: true, Example: `@fallback("app:*")`},
		{Receiver: Policy, Name: "primary", Forms: FormString, Example: `@primary("fleet:strongest")`},
	},

	// ---- Rule -----------------------------------------------------------
	lifecycle(Rule, "Operator turns reason locally first."),
	[]Placement{
		{Receiver: Rule, Name: "exclude", Forms: FormString | FormStrings, Repeatable: true, Example: `@exclude("fleet:qwen3.5:7b")`},
		{Receiver: Rule, Name: "level", Forms: FormString, Example: `@level("strong")`, Doc: "The level to resolve the call at: fast, strong, reasoning or embeddings, OVERRIDING what the call declared."},
		{Receiver: Rule, Name: "locked", Forms: FormFlag, Example: "@locked"},
		{Receiver: Rule, Name: "onUnavailable", Forms: FormString, Example: `@onUnavailable("degrade")`},
		{Receiver: Rule, Name: "policy", Forms: FormString, Example: `@policy("localFirst")`},
		{Receiver: Rule, Name: "precedence", Forms: FormNumber, Example: "@precedence(60)"},
		{Receiver: Rule, Name: "when", Forms: FormEmpty | FormKeywords, Keys: whenKeys, Example: `@when(level="fast")`},
	},

	// ---- Seed -----------------------------------------------------------
	lifecycle(Seed, "Knowledge baseline for every professional role."),
	[]Placement{
		{Receiver: Seed, Name: "namespace", Forms: FormString, Example: `@namespace("agents")`, Doc: "The namespace the seeded row's concept is resolved in, when it differs from the file's domain."},
		{Receiver: Seed, Name: "scope", Forms: FormString, Example: `@scope("perUser")`, Doc: "Seed scope: \"global\" seeds once for the cluster, \"perUser\" once for every user."},
		{Receiver: Seed, Name: "templateFile", Forms: FormString, Example: `@templateFile("templates/assistant.tmpl")`, Doc: "A template file whose rendered text is the seeded row's content."},
		{Receiver: Seed, Name: "version", Forms: FormString, Example: `@version("1.0.0")`, Doc: "Version tag for the seed."},
	},

	// ---- Concept --------------------------------------------------------
	[]Placement{
		{Receiver: Concept, Name: "composable", Forms: FormFlag | FormKeywords, Keys: composableKeys, Example: `@composable(as="ticket", fields="title,status", list="openTickets")`},
		{Receiver: Concept, Name: "description", Forms: FormString, Example: `@description("A support ticket raised by a customer.")`},
		{Receiver: Concept, Name: "displayCard", Forms: FormKeywords, Keys: displayCardKeys, Example: `@displayCard(primary="title", secondary="status")`},
		{Receiver: Concept, Name: "mirroredTo", Forms: FormString | FormStrings, Example: `@mirroredTo("shopify")`},
		{Receiver: Concept, Name: "namespace", Forms: FormString, Example: `@namespace("support")`},
		{Receiver: Concept, Name: "origin", Forms: FormString, Example: `@origin("memql")`},
		{Receiver: Concept, Name: "rowAuthz", Forms: FormKeywords, Keys: rowAuthzKeys, Example: `@rowAuthz(owner="ownerUserId", clusterOwner)`},
		{Receiver: Concept, Name: "type", Forms: FormString, Example: `@type("collection")`, Doc: "The concept's row kind: \"object\" (the default), \"collection\" or \"reference\"."},
		{Receiver: Concept, Name: "version", Forms: FormString, Example: `@version("1.0.0")`, Doc: docVersionConcept},
	},

	// ---- ConceptBody ----------------------------------------------------
	[]Placement{
		{Receiver: ConceptBody, Name: "relationship", Forms: FormKeywords, Keys: relationshipKeys, Repeatable: true, Example: `@relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")`},
	},

	// ---- ConceptField ---------------------------------------------------
	[]Placement{
		{Receiver: ConceptField, Name: "default", Forms: FormString | FormNumber | FormBool, Example: `@default("open")`, Doc: docDefaultConceptField},
		{Receiver: ConceptField, Name: "description", Forms: FormString, Example: `@description("The ticket's one-line title.")`, Doc: "The field's description, emitted into the concept schema."},
		{Receiver: ConceptField, Name: "immutable", Forms: FormFlag, Example: "@immutable"},
		{Receiver: ConceptField, Name: "internal", Forms: FormFlag, Example: "@internal"},
		{Receiver: ConceptField, Name: "maxLength", Forms: FormNumber, Example: "@maxLength(120)"},
		{Receiver: ConceptField, Name: "maximum", Forms: FormNumber, Example: "@maximum(100)"},
		{Receiver: ConceptField, Name: "minLength", Forms: FormNumber, Example: "@minLength(1)"},
		{Receiver: ConceptField, Name: "minimum", Forms: FormNumber, Example: "@minimum(0)"},
		{Receiver: ConceptField, Name: "open", Forms: FormFlag, Example: "@open"},
		{Receiver: ConceptField, Name: "pattern", Forms: FormString, Example: `@pattern("^[a-z][a-z0-9-]*$")`},
		{Receiver: ConceptField, Name: "pii", Forms: FormFlag, Example: "@pii"},
		{Receiver: ConceptField, Name: "required", Forms: FormFlag, Example: "@required"},
		{Receiver: ConceptField, Name: "secret", Forms: FormFlag, Example: "@secret", Doc: docSecretField},
		{Receiver: ConceptField, Name: "serverSet", Forms: FormFlag, Example: "@serverSet"},
		{Receiver: ConceptField, Name: "unique", Forms: FormFlag, Example: "@unique"},
		{Receiver: ConceptField, Name: "variant", Forms: FormKeywords, Keys: variantKeys, Example: `@variant(discriminator="kind")`},
	},

	// ---- ArgsField ------------------------------------------------------
	[]Placement{
		{Receiver: ArgsField, Name: "enum", Forms: FormString | FormStrings, Example: `@enum("open", "closed")`},
		{Receiver: ArgsField, Name: "maxLength", Forms: FormNumber, Example: "@maxLength(120)", Doc: "The most characters a string argument may carry (a rune count, enforced on strings)."},
		{Receiver: ArgsField, Name: "maximum", Forms: FormNumber, Example: "@maximum(60)", Doc: "The INCLUSIVE upper bound on a numeric argument (memql#4522)."},
		{Receiver: ArgsField, Name: "minimum", Forms: FormNumber, Example: "@minimum(30)", Doc: "The INCLUSIVE lower bound on a numeric argument (memql#4522)."},
		{Receiver: ArgsField, Name: "pattern", Forms: FormString, Example: `@pattern("^[A-Za-z]{2,3}$")`, Doc: "A regular expression a string argument must match, compiled once at load."},
		{Receiver: ArgsField, Name: "required", Forms: FormFlag, Example: "@required", Doc: "The caller must pass the argument. The `!` sigil after the type is the same thing."},
	},

	// ---- ToolField ------------------------------------------------------
	[]Placement{
		{Receiver: ToolField, Name: "autoInjected", Forms: FormFlag, Example: "@autoInjected"},
		{Receiver: ToolField, Name: "default", Forms: FormString, Example: `@default("5")`, Doc: "The default the tool's input schema advertises to the model."},
		{Receiver: ToolField, Name: "description", Forms: FormString, Example: `@description("Max results to return.")`, Doc: "The field's description, shown to the model in the tool's input schema."},
		{Receiver: ToolField, Name: "enum", Forms: FormString | FormStrings, Example: `@enum("exec", "fs_read")`},
		{Receiver: ToolField, Name: "required", Forms: FormFlag, Example: "@required"},
	},

	// ---- PromptField ----------------------------------------------------
	[]Placement{
		{Receiver: PromptField, Name: "default", Forms: FormString | FormNumber, Example: `@default("en")`, Doc: "The default the prompt's input schema declares for the field."},
		{Receiver: PromptField, Name: "description", Forms: FormString, Example: `@description("The episodes to distil.")`, Doc: "The field's description, carried into the prompt's input schema."},
		{Receiver: PromptField, Name: "enum", Forms: FormString | FormStrings, Example: `@enum("short", "long")`},
		{Receiver: PromptField, Name: "required", Forms: FormFlag, Example: "@required"},
	},

	// ---- BuiltinField ---------------------------------------------------
	[]Placement{
		{Receiver: BuiltinField, Name: "description", Forms: FormString, Example: `@description("The campaign to send.")`, Doc: "The field's description, read into the generated SDKs' docs."},
		{Receiver: BuiltinField, Name: "required", Forms: FormFlag, Example: "@required"},
	},
)

func concat(groups ...[]Placement) []Placement {
	var out []Placement
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// Docs maps an annotation name to its one-line hover/completion doc. A
// placement whose name means something different on its receiver carries its
// own Placement.Doc; every other placement shows this one (enforced by
// TestEveryPlacementHasADoc).
var Docs = map[string]string{
	// Lifecycle / shared.
	"enabled":            "Accepted explicit no-op: definitions are enabled by default. Use @disabled to deactivate.",
	"disabled":           "Disable this definition.",
	"description":        "Human-readable description of this definition. PREFER the /// doc-comment form (#2601): a /// block immediately above the declaration IS the description and wins over this annotation; @description remains the valid compatibility fallback -- the tree gate rejects the redundant long form (including a bare @description shadowed by a /// block). Aim for ~500 characters (editorial target).",
	"eventField":         "On an event-triggered logic: declare the allowed top-level event payload fields (e.g. @eventField(\"partitionId\", \"siParticipantId\")). Opt-in field-level validation -- every event.payload.<field> reference in the body is checked against this set at load time, rejecting typos / fields the (possibly synthetic, handler-assembled) triggering event cannot carry (memql#1743). Bare names or payload.-prefixed paths both normalize to the head segment.",
	"public":             "Per-row-authz marker: this query/mutation is intentionally callable without a caller-scope filter (concept catalogs, pre-auth login paths). See docs/public/operate/auth/per-row-authz-audit.md.",
	"requiresCapability": "The CAPABILITY a caller must hold to invoke the construct -- @requiresCapability(\"read\", \"principal\") (epic memql#5166, D11). The sibling of @requiresRank, and the difference is the point: a RANK is a floor on the cluster's ladder, a CAPABILITY is a (verb x resource) grant a role was given, and a cluster can hold one without the other -- developer ranks above admin and holds strictly fewer principal verbs. Declared together, BOTH must pass. VALIDATED AT LOAD against the five verbs and the resource kinds any role in this cluster holds a grant on, so a misspelling refuses boot rather than gating a surface into silence; ENFORCED at execution through the runtime capability catalog, on the direct call and on every plan that expands the construct. It replaces the slug-comparing specs (requiresAdmin, requiresOwnerOrAdmin, requiresDeveloperOrAbove), which could not see a custom role at all: `role == \"admin\"` is false for a rank-250 role holding every principal verb. It gates WHO MAY CALL; @rowAuthz still decides WHICH ROWS come back.",
	"requiresRank":       "The actor-rank FLOOR: only a caller holding this role, or one ranked above it, may invoke the construct -- @requiresRank(\"developer\") (epic memql#4832, D6). ENFORCED at execution and VALIDATED AT LOAD against the role ladder in dsl/rbac, so a typo refuses boot rather than gating on rank 0 and admitting everyone. This is the server-side counterpart to MemQL OS's per-surface role requirement: the shell keeps hiding what a caller cannot reach (hiding an action beats letting them click it and reading a refusal) and this makes the hidden surface a REFUSED one. Declared on the CONSTRUCT because a surface is a set of constructs and an app id from a browser is a claim, not a fact. It gates WHO MAY CALL; @rowAuthz still decides WHICH ROWS come back.",
	"serverOnly":         "Bars the construct from client-originated calls while leaving server-side Go free to call it (memql#2800). ENFORCED at execution against auth.CallOrigin -- unlike the retired @internal, which only hid a construct from discovery. Use only when caller-scoping is impossible: the auth path resolving `sub` -> user before an actor exists, or an automation acting on a user other than the actor. Callers must stamp auth.ContextWithInternalOrigin.",
	"actor":              "On a query / mutation / logic / automation: declares that the body reads the authenticated actor (actor.*). On a shape: kind marker -- projects the auth-context envelope (actor.userId / role / ...).",
	"mergeFields":        "On an update mutation: deep-merge the named object-typed payload fields into the stored object instead of replacing them wholesale, so sibling keys survive a single-key write. Format: @mergeFields(\"preferences\").",
	"appendFields":       "On an update mutation: append the named array-typed payload fields' elements to the stored array instead of replacing it wholesale, so a single-writer mutation can accumulate list items (e.g. attach one id). Format: @appendFields(\"attachmentIds\").",
	"addToSet":           docAddToSet,
	"removeFromSet":      docRemoveFromSet,
	"createOnly":         "On an insert (create-or-upsert) mutation: write the named payload fields ONLY when creating the row. If the target id already exists, the fields are dropped from the delta before the engine read-merge, so the stored value is preserved rather than clobbered -- making a deterministic-id re-stage idempotent for lifecycle fields another writer owns after creation (e.g. stageOutboundRequest seeds status but must not reset a row the outbound worker moved to sent). The inverse of @mergeFields/@appendFields: only valid on insert-kind mutations. Format: @createOnly(\"status\", \"attempts\"). See fylo#63.",
	"noUnset":            docNoUnset,
	"scrubPii":           "On an update mutation (the hard-delete / data-deletion path): after the partial payload merges, zero EVERY field the bound concept marks @pii. The field set is derived from the schema, so a newly-annotated PII field is scrubbed automatically with no change to the mutation. Bare flag, no arguments. See memql#1711.",
	// Automation.
	"trigger":  "Event trigger for automations. Format: @trigger(event=\"graph.node.created.*.v1:ns:concept\") or @trigger(schedule=\"0 0 * * * *\").",
	"filter":   "Filter expression for automation triggers.",
	"template": "On an automation: this is a work-spine TEMPLATE, invoked by a v1:work:run that named it rather than fired by the graph (memql#5048). It is the third way an automation can be reachable, alongside an event trigger and a schedule. A @template automation must carry NEITHER @trigger nor @schedule -- the load-time gate refuses both combinations, so \"called\" and \"triggered\" stay distinct.",
	"schedule": "Cron schedule for a scheduled automation. Format: @schedule(cron=\"0 0 * * * *\"). Synonym for @trigger(schedule=...); folds to the same scheduler field (#2712).",
	// Capability (memql#2218, behavioral-constructs ADR §2.3).
	"sideEffect": "On a capability: the coarse risk class @sideEffect(\"read\"|\"write\"|\"exec\"). It is the authoritative side-effect class: it lives on the capability, not on the action that calls it, and must equal the class of the Go capability the declaration names, so an authored or generated action cannot claim a lower one.",
	// Pagination opt-out (epic 5, memql#1965).
	"unbounded": "On a list-returning query: opt out of the pagination authoring rule and the implicit 50-row runtime cap. Format: @unbounded(\"reason\"). The reason string is REQUIRED -- it documents why this query is a legitimate full-set read (small bounded catalog, sweep job, etc.) and is enumerated by the pagination audit report. A query that paginates/sorts is already bounded and must NOT carry @unbounded; the engine clamps the realized window to MEMQL_MEMORY_ENGINE_MAX_WINDOW regardless. See docs/public/language/authoring-rules.md.",
	// Temporal-access visibility (core-builtins ADR §2.3, memql#2305).
	"latestMode": "On a query: marks the query as time-dependent because it reads `asOf latest` (the live tip of the append-only stream), so its result is clock-dependent / not reproducible. The engine AUTO-DERIVES this from an `asOf latest` clause in the body, so the annotation is an explicit, reader-facing restatement of that contract -- not a switch. A query with `asOf <explicit timestamp>` is deterministic and is NOT time-dependent.",
	// MCP promotion (epic memql#1529 Phase 4 #1534).
	"mcp": "Expose this construct on the MCP connector surface. On a query/mutation/automation it promotes the construct into its own first-class MCP tool (otherwise it stays reachable via the generic run_query / run_mutation / run_automation dispatchers). On a tool it opts the tool into the curated connector allowlist: once ANY tool carries @mcp, tools/list reflects only @mcp tools (otherwise -- zero tagged -- the full tool surface is reflected, so the annotation is inert until the curated set is tagged).",
	// Tool.
	"handler":              "Tool handler configuration. Format: @handler(type=\"query\", query=\"...\") / @handler(type=\"function\", name=\"...\").",
	"executionTime":        "Expected execution time hint: \"fast\", \"medium\", or \"slow\".",
	"destructive":          "Mark a tool as destructive (mutates/deletes); the tool loop gates it behind a confirmation.",
	"requiresConfirmation": "Require explicit user confirmation before the tool executes.",
	"rateLimit":            "Tool rate limiting. Format: @rateLimit(maxCalls=100, periodSeconds=3600).",
	"allowedRoles":         "Restrict the tool to a set of agent roles.",
	"scopes":               "Authorization scopes the tool requires.",
	// Builtin.
	"executor": "Go executor name for builtin functions (integration.X.Y).",
	"args":     "Parse-time argument contract for builtin functions.",
	"alias":    "Additional name the builtin is registered under. Repeatable.",
	"sdk":      "Generator marker (sdk/gen reads from source); no engine effect.",
	// Prompt.
	"defaultProvider": "Default AI provider for prompt execution: an explicit pin that wins over every routing rule.",
	"templateFile":    "A template file beside the construct: on a prompt, the .tmpl rendered with the input fields; on a seed, the text of the seeded row's content.",
	// Provider.
	"type":     "Provider vendor type (e.g., \"OpenAI\", \"Anthropic\"); on a concept, the row kind (\"object\"/\"collection\"/\"reference\").",
	"model":    "Model identifier (e.g., \"gpt-5.4-mini\", \"claude-sonnet-4-6\").",
	"modality": "Provider modality (e.g., \"chat\", \"audio\", \"image\", \"embedding\").",
	"default":  "Mark this provider as the default for its modality; on a field, the value used when the caller omits it.",
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
	"version":     "Version tag for a concept or a seed: a semver string, @version(\"1.0.0\"). Metadata only -- canonical ids are not versioned by it (#2613).",
	"namespace":   "Concept namespace. DEFAULTS to the containing dsl/<domain>/ directory (#2614) -- write it only for a colon-scoped sub-namespace (\"cognition:client:tool\") or a pinned divergence (namespace.pin). An explicit value must equal the directory, extend it as <dir>:..., or match the domain pin; any other mismatch is a load error (the moved-file guard: file location is id-bearing, so moving a .memql file between domains changes canonical ids).",
	"scope":       "On a seed: \"global\" seeds once for the cluster, \"perUser\" once for every user. (On a concept @scope is retired -- every concept lives in the default partition, #56.)",
	"cache":       "Override the result-cache TTL for the query. Preferred form (#2618): @cache(300) -- the single ttl arg makes position unambiguous. The keyword form @cache(ttl=\"300\") keeps parsing. Pure reads cache BY DEFAULT (a 60s backstop) without this annotation; @cache sets a different TTL, longer or shorter. @cache(ttl=\"0\") is the explicit \"never cache\" opt-out (or use @nocache). The engine keys the cache on the plan signature (query/sort/limit/depth/shape + the keyset cursor) and evicts on any write to the read concept via the cache.invalidate.* broadcast channel -- cross-node eviction needs no per-concept routing rule.",
	"nocache":     "Opt this query OUT of caching entirely (force \"never cache\"). Clearer alias for @cache(ttl=\"0\"); use it for reads that must always be live (auth, monotonic counters, presence). Pure reads cache by default (5.6), so @nocache is the escape for the rare read where even brief staleness is wrong.",
	"displayCard": "Rendering hints for concept-agnostic clients (memql#160): the field shown as a row's title (primary=, required) and the fields for the secondary, tertiary and status slots. Each must be a displayable field the concept declares, checked once the property set is known.",
	"composable":  "The Materializer's mark (epic memql#4977, D2): this concept's rows are worth composing a file FROM. Bare, it takes the defaults; as= names the row kind, fields= lists the fields to compose from and list= names the query that lists the rows.",
	// Two independent axes: `type` is what the ENGINE DOES with the edge (a
	// closed set), `as` is what the edge MEANS (open, form-validated only).
	// The target is a BARE concept name resolved through a file-top `use`
	// import -- the quoted canonical-ID form this doc used to show was retired
	// by memql#1067, so the editor was teaching an author the one spelling the
	// conformance gate rejects (memql#3661).
	"relationship": "Foreign-key relationship metadata. Format: @relationship(type=\"parent\", field=\"x\", target=concept, direction=\"outgoing\"), plus an optional as=\"domainVerb\" label.",
	"rowAuthz":     docRowAuthz,
	// Data origins (epic memql#4378). Two declarations, three derived
	// states, no fourth.
	"origin":     "Declares WHERE CHANGES TO THIS CONCEPT ARE MADE -- the system that owns the data. @origin(\"memql\") (the default when the annotation is absent) means MemQL originates it; @origin(\"<connector>\") names an external system, which makes the concept a MIRROR. A mirror is READ-ONLY BY CONSTRUCTION: component/memql refuses every write to it -- mutation, tool handler, raw insert or staged write -- that does not come from the connector the origin names, so what the badge says is what a reader may assume. The name must be a registered connector or the engine REFUSES BOOT naming the concept: a mirror nobody fills is a lie. Pairs with @mirroredTo to derive dataState (mirror | origin | native), which the registry, both SDKs and the portal badge read. See docs/public/concepts/data-origins.md.",
	"mirroredTo": "Declares WHO ELSE HOLDS A COPY of this MemQL-origin concept: @mirroredTo(\"shopify\"), or several names in one annotation. Every write to the concept appends one v1:platform:outboxEntry per target, in the write's own transaction, which a per-connector drain worker delivers with an idempotency key, backoff, dead-lettering and audit. Only valid on a MemQL-origin concept -- @mirroredTo beside an external @origin is REFUSED at load, because re-mirroring somebody else's data onward is the origin's job and a mirror that also publishes is a second origin wearing the first one's badge. Each named connector must be registered or the engine refuses boot: a mirror target nobody drains is a silent drop. See docs/public/concepts/data-origins.md.",
	// Fields.
	"required":     "The field is required: a concept write without it fails the schema, and a caller must pass an args / tool / prompt / builtin field that carries it. The `!` sigil after the type is the same thing.",
	"enum":         "The closed set of string values the field accepts: @enum(\"a\", \"b\"). The `enum(\"a\", \"b\")` type is the same constraint in one statement.",
	"pattern":      "A regular expression a string value must match.",
	"minLength":    "The fewest characters a string value may carry.",
	"maxLength":    "The most characters a string value may carry.",
	"minimum":      "The INCLUSIVE lower bound on a numeric value.",
	"maximum":      "The INCLUSIVE upper bound on a numeric value.",
	"unique":       "Declared metadata (memql#2960): emitted as x-unique in the concept schema; nothing enforces uniqueness.",
	"immutable":    "Declared metadata (memql#2960): emitted as x-immutable in the concept schema; nothing refuses a later write to the field.",
	"secret":       "The field holds a secret: emitted as x-secret, and every validation surface that quotes a rejected value redacts it (memql#3036). Not a secrecy guarantee -- see Concept.SecretFields for the limits.",
	"pii":          "The field is personally identifying: emitted as x-pii, and the hard-delete scrub (@scrubPii, memql#1711) zeroes every such field generically.",
	"serverSet":    "The field is stamped server-side (createdAt, createdBy, status, ...): never accepted from a mutation's caller args, but projected like any other field. Emitted as x-serverSet (memql#2035).",
	"open":         "On a nested object block: the block accepts keys it does not declare, suppressing the closed-by-default additionalProperties: false (memql#3641). For a block whose keys are data rather than schema.",
	"variant":      "A discriminated union: @variant(discriminator=\"kind\") on an object field, followed by one block per branch; the discriminator field's value picks the branch.",
	"internal":     "On a concept field: server-only (memql#2035) -- never projected by a shape's default projection and never accepted from a mutation's caller args; emitted as x-internal. (On a construct, @internal is retired, #2708.)",
	"autoInjected": "The field is filled by the runtime rather than the model: it is kept out of the input schema the model sees and stamped server-side (the calling agent's id, for example).",
}
