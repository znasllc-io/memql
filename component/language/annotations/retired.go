package annotations

// retired.go holds the annotations an author may still write from habit and
// the pointed message each one earns. They moved here from core/baseparser
// (the construct-level retirements and the @row hint) and from the
// per-construct parsers and loaders (@kind / @sideEffect / @reliability on an
// action, @clientExecution on a tool, @shape on a spec, @concepts / @caller on
// a shape, @scope / @cache on a concept, @default / @description on an args
// field), so every gate gives the same answer about them.
//
// A retired name is consulted only after the receiver has refused the name:
// a name retired on constructs but live on a field (@internal on a concept
// field) is accepted there.

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
}

// retiredOn maps an annotation retired on one receiver (and usually live on
// another) to its migration hint.
var retiredOn = map[retiredKey]string{
	{Action, "kind"}:           "composites are automations now and a primitive needs no marker (construct-invocation ADR Decision 3); remove it",
	{Action, "sideEffect"}:     "the authoritative side-effect class lives on the CAPABILITY declaration now (ADR Decision 3, Story 5); remove it from the action",
	{Action, "reliability"}:    "reliability is machine-managed runtime state, not source (ADR Decision 3); remove it",
	{Tool, "clientExecution"}:  "it dispatched the tool to the connected browser over the client-tool relay, which was removed with the cognition node (epic memql#4988). Every tool now needs a server-side @handler",
	{Spec, "shape"}:            "a spec binds its shape or concept in the signature (epic #2281): `spec <boundName> <name> { return <bool> }`, with boundName resolved through the file-top `use` import",
	{Shape, "concepts"}:        "bind the concept in the signature instead: `shape <Concept> <name> { ... }`, with the concept imported by a file-top `use` line",
	{Shape, "caller"}:          "use @actor (#221); the field accessor was renamed at the same time (caller.X -> actor.X)",
	{Concept, "scope"}:         "remove the annotation; every concept lives in the default partition post-#56",
	{Concept, "cache"}:         "a concept carries no cache setting -- a read caches, so set the TTL on the query that reads the concept (@cache(300)); a write to the concept evicts every cached read of it",
	{ArgsField, "default"}:     "it is never applied; write `args.<field> ?? <default>` in the body (a concept-field @default is not a substitute -- it is never applied on insert either)",
	{ArgsField, "description"}: "it was never retained (no AST slot); document the field with a `///` doc comment on the line above it (memql#3336)",
}

// useFamilyHint is the hint for the retired @use* binding family
// (@useConcept, @useShape, @useQuery, ...): a prefix rule rather than a list,
// so a member nobody listed is refused the same way.
const useFamilyHint = "declare the dependency with a file-top `use <module>.{ ... }` import instead, and put a bound concept in the signature (`query <Concept> <name> { ... }`)"

// isUseFamily reports whether name is a member of the retired @use* family:
// "use" followed by an upper-case letter.
func isUseFamily(name string) bool {
	return len(name) > 3 && name[:3] == "use" && name[3] >= 'A' && name[3] <= 'Z'
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
