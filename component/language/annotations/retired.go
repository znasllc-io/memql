package annotations

import "sort"

// retired.go holds the annotations an author may still write from habit and
// the pointed message each one earns. They moved here from core/baseparser
// (the construct-level retirements and the @row hint) and from the
// per-construct parsers and loaders (@kind / @sideEffect / @reliability on an
// action, @clientExecution on a tool, @shape / @row / @actor on a spec, @concepts / @caller on
// a shape, @scope / @cache on a concept, @default / @description on an args
// field), so every gate gives the same answer about them.
//
// A retired name is consulted only after the receiver has refused the name:
// a name retired on constructs but live on a field (@internal on a concept
// field) is accepted there.

// AttributeRewriteHint is the one sentence every epic memql#5375 retirement
// hint ends with. A hint that explains the retirement without naming the
// migrator leaves the author to go and find it, which is the step the
// sentence exists to remove. Retirements that predate the epoch (@internal,
// @role, @permission) deliberately omit it: deleting the annotation is their
// whole migration, so the command has nothing to do for them.
const AttributeRewriteHint = "run `memqlmigrate --rewrite=attributes`"

// retiredKey names one retirement that applies to one receiver.
type retiredKey struct {
	receiver Receiver
	name     string
}

// retiredEverywhere maps an annotation retired on every receiver that does
// not accept it to its migration hint.
var retiredEverywhere = map[string]string{
	"internal":   "retired under the 2026.08 epoch (#2620 ruling / #2708); it only hid the construct from external discovery surfaces (tool listing, MCP promotion, the help()/listFunctions internal flag) while leaving it callable -- delete the annotation",
	"role":       "buried (#2631 ruling / #2709); it was documented but never enforced (nothing ever checked the value at runtime; the load gate rejects it) -- access control lives at the actor layer (RBAC + the @public per-row-authz classification)",
	"permission": "buried (#2631 ruling close-out / #2713); the @role twin -- documented but never enforced (its one help-payload reader was dead; the load gate rejects it) -- access control lives at the actor layer (RBAC + the @public per-row-authz classification)",
	"visibility": "removed in the genesis simplification; it chose which node types load a construct, and every binary now loads everything while build tags decide which integrations are active -- delete the annotation",
	// The function annotations #989 took out of the allow-lists. Nothing has
	// read any of them since; naming the history here is what turns a bare
	// "unknown annotation" into a refusal that says what happened to it.
	"timeout":    "removed from the allow-lists in memql#989; nothing reads it -- delete the annotation; " + AttributeRewriteHint,
	"retry":      "removed from the allow-lists in memql#989; nothing reads it -- delete the annotation; " + AttributeRewriteHint,
	"audit":      "removed from the allow-lists in memql#989; nothing reads it -- delete the annotation; " + AttributeRewriteHint,
	"idempotent": "removed from the mutation allow-list in memql#989; nothing reads it -- delete the annotation; " + AttributeRewriteHint,
	// epic memql#5375. Each was parsed, stored and read by nothing that
	// changes behaviour, or was the losing spelling of a pair.
	"unique":     "memql#5375: declared metadata with no uniqueness check behind it (memql#2960), so it read as a constraint while constraining nothing -- delete it; " + AttributeRewriteHint,
	"immutable":  "memql#5375: declared metadata with no write guard behind it -- delete it; a field that must not change is enforced by the mutation that writes it; " + AttributeRewriteHint,
	"deprecated": "memql#5375: it was rendered by help() and editor hover and read by nothing that changes behaviour -- delete it, or say so in the construct's @description; " + AttributeRewriteHint,
	"latestMode": "memql#5375: the engine derives time-dependence from `asOf latest` in the body, so the annotation restated it and could contradict it -- delete it; " + AttributeRewriteHint,
	"enabled":    "memql#5375: constructs are enabled by default, so @enabled was an explicit no-op that read like a switch -- delete it, and use @disabled to deactivate; " + AttributeRewriteHint,
	"nocache":    "memql#5375: write @cache(0) -- one annotation for the cache TTL, with 0 meaning never; " + AttributeRewriteHint,
	"schedule":   "memql#5375: write @trigger(schedule=\"0 0 * * * *\") -- one annotation declares how an automation is reached, which is what lets @template be refused beside it coherently; " + AttributeRewriteHint,
	"rateLimit":  "memql#5375: Tool.RateLimit was cloned and copied into a Function field nothing reads, so the declared ceiling did not exist -- delete it; the live ceilings are the provider chokepoint in ai_guard.go and the run budget in component/work; " + AttributeRewriteHint,
	"scopes":     "memql#5375: Tool.Scopes was advertised on the gRPC tool descriptor and checked nowhere, so it read as an authorization gate while gating nothing -- delete it; use @requiresCapability for a real one; " + AttributeRewriteHint,
	"namespace":  "memql#5375: a concept's namespace is its domain directory, or that directory's one-line namespace.pin -- the annotation could only restate one of those or silently disagree with it; delete it, and pin a deliberate divergence with a namespace.pin file (#2614); " + AttributeRewriteHint,
	"async":      "refused on automations since memql#2712: an automation already runs asynchronously off its event or schedule trigger, and nothing reads the annotation -- delete it",
}

// retiredOn maps an annotation retired on one receiver (and usually live on
// another) to its migration hint.
var retiredOn = map[retiredKey]string{
	{Action, "kind"}:           "an action is always one primitive capability call now, and a composite of several is an automation, so there is nothing to mark; remove it",
	{Action, "sideEffect"}:     "the authoritative side-effect class lives on the capability declaration the action calls, where an action cannot overstate or understate it; remove it from the action",
	{Action, "reliability"}:    "reliability is runtime state the engine keeps, not something the source declares; remove it",
	{Tool, "clientExecution"}:  "it dispatched the tool to the connected browser over the client-tool relay, which was removed with the cognition node (epic memql#4988). Every tool now needs a server-side @handler",
	{Spec, "shape"}:            "a spec binds its shape or concept in the signature (epic #2281): `spec <boundName> <name> = row => <predicate>`, with boundName resolved through the file-top `use` import",
	{Spec, "row"}:              "it is a shape-only marker since epic #2281 -- to predicate on row metadata, bind a @row shape in the signature (`spec <shape> <name>`) and read its projected key by bare name",
	{Spec, "actor"}:            "it is a shape-only marker since epic #2281 -- to predicate on the caller, bind an @actor shape in the signature (`spec <shape> <name>`) and read its projected key by bare name",
	{Shape, "concepts"}:        "bind the concept in the signature instead: `shape <Concept> <name> { ... }`, with the concept imported by a file-top `use` line",
	{Shape, "caller"}:          "use @actor (#221); the field accessor was renamed at the same time (caller.X -> actor.X)",
	{Concept, "scope"}:         "remove the annotation; every concept lives in the default partition post-#56",
	{Automation, "schedule"}:   "a scheduled automation is written @trigger(schedule=\"<cron>\"), the one spelling (D15, epic memql#5370); memqlmigrate --rewrite=bodies rewrites it",
	{Concept, "cache"}:         "a concept carries no cache setting -- a read caches, so set the TTL on the query that reads the concept (@cache(300)); a write to the concept evicts every cached read of it",
	{ConceptField, "default"}:  "memql#5375: it is never applied on insert -- it was published as the JSON-Schema `default` keyword, which no validator applies; write `args.<field> ?? <default>` in the mutation body. It stays live on a tool / prompt / builtin field, where the body IS the schema the model reads; " + AttributeRewriteHint,
	{ArgsField, "default"}:     "it is never applied; write `args.<field> ?? <default>` in the body (a concept-field @default is not a substitute -- it is never applied on insert either)",
	{ArgsField, "description"}: "it was never retained (no AST slot); document the field with a `///` doc comment on the line above it (memql#3336)",
}

// useFamilyHint is the hint for the retired @use* binding family
// (@useConcept, @useShape, @useQuery, ...): a prefix rule rather than a list,
// so a member nobody listed is refused the same way.
const useFamilyHint = "declare the dependency with a file-top `use <module>.{ ... }` import instead, and put a bound concept in the signature (`query <Concept> <name> { ... }`)"

// useFamilyPrefix is what every member of the retired @use* family starts
// with, before its upper-case letter.
const useFamilyPrefix = "use"

// isUseFamily reports whether name is a member of the retired @use* family:
// "use" followed by an upper-case letter.
func isUseFamily(name string) bool {
	n := len(useFamilyPrefix)
	return len(name) > n && name[:n] == useFamilyPrefix && name[n] >= 'A' && name[n] <= 'Z'
}

// Retirement is one retired annotation as the tables above record it: where
// it is retired and the hint its refusal carries. It is a read-only view for
// the generated attribute matrix, which lists every retirement; Check reads
// the tables themselves.
type Retirement struct {
	// Receiver is where the name is retired. Empty means every receiver that
	// does not accept the name (retiredEverywhere).
	Receiver Receiver
	// Name is the retired name. When Prefix is set it is the prefix of a
	// retired family instead: every name that continues it with an upper-case
	// letter is retired (the @use* family: @useConcept, @useShape, ...).
	Name   string
	Prefix bool
	// Hint is the migration hint the refusal carries.
	Hint string
}

// Retirements returns every retirement, sorted by name. A name retired
// everywhere comes before its receiver-specific entries, which follow in
// receiver order. The result is a fresh slice the caller may keep.
func Retirements() []Retirement {
	out := make([]Retirement, 0, len(retiredEverywhere)+len(retiredOn)+1)
	for name, hint := range retiredEverywhere {
		out = append(out, Retirement{Name: name, Hint: hint})
	}
	for key, hint := range retiredOn {
		out = append(out, Retirement{Receiver: key.receiver, Name: key.name, Hint: hint})
	}
	out = append(out, Retirement{Name: useFamilyPrefix, Prefix: true, Hint: useFamilyHint})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return retirementRank(out[i]) < retirementRank(out[j])
	})
	return out
}

// retirementRank orders the entries of one name: everywhere first, then the
// receivers in receiver order.
func retirementRank(r Retirement) int {
	if r.Receiver == "" {
		return -1
	}
	return receiverIndex[r.Receiver]
}

// retiredHint returns the migration hint for name on r, or false when name is
// not retired there.
func retiredHint(r Receiver, name string) (string, bool) {
	if hint, ok := retiredOn[retiredKey{r, name}]; ok {
		return hint, true
	}
	if hint, ok := retiredEverywhere[name]; ok {
		return hint, true
	}
	if isUseFamily(name) {
		return useFamilyHint, true
	}
	return "", false
}

// RetiredHint reports the migration hint for a retired annotation on r, and
// whether it is retired there at all. It is the exported view of the tables
// for callers outside this package -- the load gates read them through Check;
// this is for a test or a tool that needs to name the hint it expects.
func RetiredHint(r Receiver, name string) (string, bool) { return retiredHint(r, name) }

// misplacedHints adds a pointed sentence to the misplaced refusal of a name
// whose usual mistake needs more than "it is accepted on a shape" (memql#2779).
//
// `@row` is the motivating case: an author who wants to filter on the row
// envelope reaches for `@row` on the query, because that is how a SHAPE opts
// into the row surface. A query needs no kind marker -- it binds its concept
// in the signature, and `row.` is available in its filter unconditionally.
var misplacedHints = map[string]string{
	"row": "`@row` is a SHAPE kind marker -- it declares that a shape body projects the row envelope (`shape <Concept> <name> { row.id ... }`). A query needs no kind marker: it binds its concept in the signature (`query <Concept> <name>`), and the `row.` namespace is always available in its filter. To filter on the row id, delete the annotation and write `filter row.id == args.<x>`",
}
