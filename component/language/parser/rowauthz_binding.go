package parser

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The #2920 row-authorization declaration surface: who may see a
// concept's rows becomes a property of the CONCEPT, declared once, in
// place of the `actor.*` term an author must remember to type into
// every filter over it (#2803).
//
// THIS FILE IS THE DECLARATION SURFACE, AND THE TIER IT PRODUCES IS
// ENFORCED. Parsing, validating and rewriting is all that happens
// here, which is easy to mistake for the whole story -- it WAS the
// whole story in Phase 1 (#2920), and this header went on saying so
// for months after it stopped being true (swept in memql#3987). Phase
// 3 ended it. component/memql/rowauthz_enforce.go ANDs the tier's
// predicate into the plan of every read with a bound concept, admits
// each emitted row against the tier its own concept declares (the only
// mechanism available to a raw client-supplied query string or to
// graph expansion, neither of which has a filter to AND anything
// into), and REFUSES a read carrying no actor rather than comparing
// against ""; memql#3174 added the write guard. So a construct over a
// declared concept does NOT return the same N rows it returned before
// the annotation landed, and the predicate column in the table below
// describes what gets applied rather than what the vocabulary was
// designed against.
//
// The build-time gate is TestRowAuthzEnforcementLandGate
// (component/memql/rowauthz_enforce_gate_test.go), which re-derives
// that no construct's result set narrows silently. It replaced the
// Phase 1 gate this header used to cite as live; that retirement is
// recorded once, in
// component/database/memory-nodes/concept_rowauthz_test.go, where the
// deleted test stood.
//
// The detector and the codemod live in one file for the #2621 reason:
// the loader-side validator (component/database/memory-nodes) and the
// memqlmigrate rewrite must share ONE definition of what a declaration
// means, or they drift into disagreeing about the same annotation.
// The codemod never parses `@rowAuthz` itself -- it renders through
// FormatRowAuthz and reads through ParseRowAuthz, the same two
// functions the loader calls.

// RowAuthzAnnotation is the annotation name, spelled once.
const RowAuthzAnnotation = "rowAuthz"

// RowAuthzSelfOwnedField is the owner value naming the row's OWN identity
// rather than a payload field -- `@rowAuthz(owner="id")`, the self-owned form
// (memql#3029, ruled in memql#2803).
//
// It exists as a constant because separate packages have to agree on it: the
// loader's validateRowAuthz (which admits it without a declared property) and
// shadow mode (which both renders `row.id == actor.userId` and matches that
// spelling back, rather than a bare field term). A string literal in several
// places is how they drift into disagreeing about one annotation -- the same
// reason the detector and the codemod share this file.
//
// The owner-field provenance gate (component/memql/rowauthz_owner_provenance.go)
// asks "can a caller write the row's id?" and compares the LITERAL "id" instead
// of naming this symbol -- and the reason for that has EXPIRED. Phase 1 policed
// the row-authz surface with an allow-list of the files permitted to touch it at
// all, so importing this constant there would have demanded an entry, and a
// static analyzer a test drives did not belong on a list about ENFORCEMENT.
// memql#3172 retired that allow-list in the commit that landed enforcement. The
// duplicated literal is therefore leftover rather than a deliberate exception
// (memql#3987); it is named here in prose so the two spellings remain findable
// from each other, not because an argument still holds it apart.
const RowAuthzSelfOwnedField = "id"

// RowAuthzTier is the declared row-authorization tier of a concept.
// The four values are the buckets docs/public/operate/auth/
// per-row-authz-audit.md already defines in prose; this type is what
// makes an author DECLARE one instead of a regex inferring it per
// construct.
type RowAuthzTier string

const (
	// RowAuthzOwned -- rows belong to a user. Injects
	// `<Owner>==actor.userId` (`row.id==actor.userId` for the
	// self-owned form), and a read carrying no actor is REFUSED
	// rather than compared against "".
	RowAuthzOwned RowAuthzTier = "owned"
	// RowAuthzClusterOwner -- rows are administrative. Injects
	// `actor.isClusterOwner==true`.
	RowAuthzClusterOwner RowAuthzTier = "clusterOwner"
	// RowAuthzPublic -- rows are globally readable by intent. Injects
	// nothing, and must be spelled explicitly: "no annotation" and
	// "declared public" are different states, which is the whole
	// point of the tier existing (#2803).
	RowAuthzPublic RowAuthzTier = "public"
	// RowAuthzGranted -- visibility comes from a relationship.
	// Injects the named spec, gated on actor.userId. Zero concepts
	// declare it today, so it has no live callers -- which is a fact
	// about the corpus, not about the mechanism.
	RowAuthzGranted RowAuthzTier = "granted"
)

// RowAuthzDecl is the parsed meaning of one `@rowAuthz(...)`
// annotation. Exactly one of Owner / Spec is set, and only for the
// tier that takes a parameter.
type RowAuthzDecl struct {
	Tier RowAuthzTier `json:"tier"`
	// Owner is the payload field compared against actor.userId.
	// Set only for RowAuthzOwned.
	Owner string `json:"owner,omitempty"`
	// Spec is the relationship spec that grants visibility.
	// Set only for RowAuthzGranted.
	Spec string `json:"spec,omitempty"`
	// ClusterOwnerBypass widens the owned tier to "the owner, OR a
	// cluster owner" -- the COMPOSITE form
	// `@rowAuthz(owner="<field>", clusterOwner)` (memql#4312).
	//
	// It is a FLAG ON THE OWNED TIER rather than a fifth RowAuthzTier
	// value, and that is the load-bearing choice. "Who owns this row"
	// must keep exactly one answer: every site that resolves an owner
	// field -- the loader's owner-property validation
	// (component/database/memory-nodes/concept_parser.go), the insert
	// stamp (component/memql/rowauthz_insert_stamp.go), the actorless-read
	// refusal, the conformance classifier's owned-tier bucket -- switches
	// on Tier == RowAuthzOwned, and a new tier value would have silently
	// fallen out of all four while looking like a tidy addition. The
	// bypass is purely ADDITIVE: it ORs the cluster-owner gate onto the
	// owner comparison and changes nothing else.
	//
	// Set only for RowAuthzOwned. On any other tier it is meaningless and
	// FormatRowAuthz refuses it rather than dropping it silently.
	ClusterOwnerBypass bool `json:"clusterOwnerBypass,omitempty"`

	// RankVisible widens the owned tier's READ half to "the owner, OR
	// anyone whose rank is at or above the OWNER'S rank" -- epic
	// memql#4832 D2, `@rowAuthz(owner="<field>", rankVisible)`.
	//
	// Peers are included at every rung: the comparison is `<=`, not `<`.
	// An owner field that resolves to no live principal -- empty, or a
	// synthetic `system:` id -- is NOT admitted by this branch. It has no
	// rank to compare, and admitting it would make every unowned row
	// visible to the whole cluster the moment a concept opted in. Use the
	// Unowned floor below to say what an unowned row is worth.
	//
	// A flag on the owned tier for the same reason ClusterOwnerBypass is
	// one: four sites switch on Tier == RowAuthzOwned, and a new tier
	// value falls silently out of all four.
	RankVisible bool `json:"rankVisible,omitempty"`

	// RankStrict widens the owned tier's WRITE half to "your own row, OR
	// a row owned by someone STRICTLY below your rank" -- D3.
	//
	// It both WIDENS and NARROWS, and the narrowing is the point. It
	// widens because the owned tier admits only the owner today, so a
	// developer cannot touch a reader's row at all. It narrows because a
	// CLUSTER OWNER writing a PEER owner's row stops being admitted by
	// rowAuthzWriteEscape -- "owner-to-owner is read-only" is D3's sharp
	// edge and the reason ownership transfer exists (memql#4838).
	//
	// Requires RankVisible. The reverse combination -- write a row you
	// cannot read -- is the one incoherent pairing, and ParseRowAuthz
	// refuses it rather than resolving it.
	RankStrict bool `json:"rankStrict,omitempty"`

	// Unowned is the role slug from which a row with an EMPTY owner field
	// becomes readable -- `@rowAuthz(owner="<field>", unowned="developer")`.
	//
	// An unowned row is the DEPLOYMENT's, not a principal's: the `self`
	// account and the portal site are the cases. Today such a row is
	// reachable through the cluster-owner branch alone, which is why an
	// admin or a developer opening the Accounts registry gets zero rows
	// and cannot tell an unconfigured cluster from an empty one
	// (memql#4837).
	//
	// NAMED `unowned` AND NOT `clusterOwned`, deliberately. It sits beside
	// `clusterOwner` in the same argument list and the two say opposite
	// halves of a sentence -- `clusterOwner` is about the ACTOR, this is
	// about the ROW -- so a one-letter difference between them would be a
	// reading trap in the one place in the tree that must be read exactly.
	//
	// Set only for RowAuthzOwned. The slug is resolved against the role
	// ladder at LOAD, so a typo refuses boot rather than silently gating
	// on rank 0 (which admits everyone).
	Unowned string `json:"unowned,omitempty"`

	// RequiresIdentity narrows the PUBLIC tier from "anybody at all,
	// including a stranger who has not signed in" to "any authenticated
	// caller of this cluster" -- `@rowAuthz(public, requiresIdentity)`
	// (memql#4809).
	//
	// IT IS THE FIRST MODIFIER THAT NARROWS RATHER THAN WIDENS, and that
	// is what it is for. Every other tier answers a question ABOUT THE ROW
	// -- who owns it, which relationship reaches it, is the caller the
	// cluster owner. Some concepts have no such question to answer: a
	// seeded catalog is the same population for every caller, with no owner
	// field and nowhere to get one. `public` is the only existing tier that
	// injects no predicate, and so the only one that fits -- but `public`
	// also means "an anonymous reader may have this", which is a different
	// and much larger claim than the author of such a catalog is making.
	//
	// So the flag says the part `public` alone cannot: there is no row-level
	// distinction to draw here, AND that is not an invitation to the
	// internet. Authorization for such a concept lives at the SURFACE
	// instead, where `@requiresRank` puts it (D6) -- which is the honest
	// place for it, because "who may ask this question" is the only
	// question a catalog row leaves open.
	//
	// A FLAG ON THE PUBLIC TIER rather than a fifth RowAuthzTier value, for
	// exactly the reason ClusterOwnerBypass and RankVisible are flags: sites
	// switch on `Tier == RowAuthzPublic` (rowauthz_write_guard.go,
	// rowauthz_enforce.go, rowauthz_shadow.go, rowauthz_anonymous.go) and a
	// new tier value would fall silently out of every one of them while
	// looking like a tidy addition. The narrowing is purely SUBTRACTIVE and
	// lands at ONE site -- conceptDeclaresPublicTier, the single function
	// that decides anonymous reach -- and changes nothing else: the tier
	// still injects no predicate, still stamps no owner, still guards no
	// write.
	//
	// Set only for RowAuthzPublic. On any other tier it is meaningless and
	// FormatRowAuthz refuses it rather than dropping it silently.
	RequiresIdentity bool `json:"requiresIdentity,omitempty"`
}

// The declaration forms. The two parameterised tiers use the house
// keyword-arg spelling (`@relationship(field="x")`,
// `@displayCard(primary="name")`) rather than the `owner: field` form
// #2803's table sketched: `:` inside an annotation argument list lexes
// as TokenColon, while parseAttribute's named-arg branch tests for a
// TokenOperator, so the colon form does not parse and degrades to a
// flag before failing on `expected ')'`. Changing the shared lexer for
// one annotation buys nothing the `=` form does not already give.
//
//	@rowAuthz(public)
//	@rowAuthz(clusterOwner)
//	@rowAuthz(owner="ownerUserId")
//	@rowAuthz(via="spaceMember")
//	@rowAuthz(owner="ownerUserId", clusterOwner)
//
// The last is the COMPOSITE form (memql#4312) and the only argument
// list with two entries. It is not a fifth tier: it is the owned tier
// carrying ClusterOwnerBypass, so "the owner, or a cluster owner". The
// two arguments are order-independent because an attribute's argument
// list is a MAP -- there is no order to depend on, and accepting one
// spelling and not the other would be a lie about the grammar.
//
// The flag tiers and the keyword tiers occupy separate namespaces
// inside the argument list, which is what leaves room for the escape
// hatch #2920 requires the grammar to be ABLE to express without
// implementing: the pre-actor bootstrap path. `userByIdSystem`
// resolves `sub` -> user in order to BUILD the actor, so `actor.userId`
// is circular there -- component/auth/identity_resolver.go:79, and the
// query is @serverOnly for exactly that reason (#2800).
//
// NOT `userById`, which this comment named until memql#2984. That is a
// different query, gated by requiresOwnerOrAdmin, and a reader who
// followed the citation found a gated construct and concluded the
// constraint was imaginary. The constraint is real; only the name was
// wrong. A fifth tier
// is a new flag name or a new keyword; neither disturbs the four here.
const (
	rowAuthzArgOwner = "owner"
	rowAuthzArgVia   = "via"
)

// rowAuthzFlagTiers maps the bare-flag spellings to their tier.
var rowAuthzFlagTiers = map[string]RowAuthzTier{
	"public":       RowAuthzPublic,
	"clusterOwner": RowAuthzClusterOwner,
}

// rowAuthzKeywordTiers maps the keyword-arg spellings to their tier.
var rowAuthzKeywordTiers = map[string]RowAuthzTier{
	rowAuthzArgOwner: RowAuthzOwned,
	rowAuthzArgVia:   RowAuthzGranted,
}

// rowAuthzSpellings renders the accepted forms for a diagnostic, in a
// stable order.
func rowAuthzSpellings() string {
	return `@rowAuthz(public), @rowAuthz(public, requiresIdentity), @rowAuthz(clusterOwner), @rowAuthz(owner="<field>"), @rowAuthz(via="<spec>"), @rowAuthz(owner="<field>", clusterOwner), @rowAuthz(owner="<field>", rankVisible[, rankStrict][, unowned="<role>"][, clusterOwner])`
}

// rowAuthzOwnedModifiers is THE set of arguments that may accompany an
// `owner=` argument. Every one of them widens the owned tier; none of
// them replaces it, which is why they are flags on one tier rather than
// tiers of their own (see RowAuthzDecl.ClusterOwnerBypass).
//
// The map's VALUE says whether the argument is a bare flag. A keyword
// modifier (`unowned="developer"`) carries a quoted value; a flag
// modifier (`clusterOwner`) must be bare, because the parser stores a
// bare identifier as `true` and `clusterOwner="yes"` is a different
// shape that is not a spelling of anything.
var rowAuthzOwnedModifiers = map[string]bool{
	rowAuthzArgClusterOwner: true,
	rowAuthzArgRankVisible:  true,
	rowAuthzArgRankStrict:   true,
	rowAuthzArgUnowned:      false,
}

// rowAuthzArgClusterOwner is the flag spelling that, BESIDE an `owner=`
// argument, forms the composite tier. Named as a constant because two
// places have to agree on it: the composite branch below, and the
// flag-tier map where the same word means the standalone tier.
const rowAuthzArgClusterOwner = "clusterOwner"

// The rank modifiers (epic memql#4832). Constants for the same reason
// rowAuthzArgClusterOwner is one: the parser, the formatter and the
// modifier table all have to agree on the spelling.
const (
	rowAuthzArgRankVisible = "rankVisible"
	rowAuthzArgRankStrict  = "rankStrict"
	rowAuthzArgUnowned     = "unowned"
)

// rowAuthzArgRequiresIdentity is the flag spelling that, BESIDE the bare
// `public` flag, narrows the public tier to authenticated callers
// (memql#4809). A constant for the reason its siblings are: the parser,
// the formatter and the modifier table all have to agree on the spelling.
const rowAuthzArgRequiresIdentity = "requiresIdentity"

// rowAuthzPublicModifiers is THE set of arguments that may accompany the
// bare `public` flag. One entry today, and the map exists rather than an
// `if` for the same reason its owned sibling does: the next modifier is a
// line here plus a branch in the formatter, not a new parse shape.
//
// The VALUE says whether the argument is a bare flag, exactly as in
// rowAuthzOwnedModifiers -- `requiresIdentity="yes"` is a different shape
// and is not a spelling of anything.
var rowAuthzPublicModifiers = map[string]bool{
	rowAuthzArgRequiresIdentity: true,
}

// parseRowAuthzPublicModifiers reads the public tier carrying its one
// narrowing modifier:
//
//	@rowAuthz(public, requiresIdentity)                       memql#4809
//
// The SECOND accepted multi-argument list, and the rule its owned sibling
// states is unchanged by it: every accepted shape names ONE tier plus
// arguments that qualify it. Two TIERS in one list is still an ambiguous
// declaration this parser must never resolve by picking a side -- which is
// why `public` is required to be present and bare here, rather than the
// modifier being accepted on its own.
//
// Returns (nil, "", false) when the args are not that shape, so the caller
// falls through to its shared diagnostic; (nil, reason, false) when the
// shape IS public-with-modifiers and one argument is wrong.
func parseRowAuthzPublicModifiers(args map[string]any) (*RowAuthzDecl, string, bool) {
	raw, hasPublic := args["public"]
	if !hasPublic {
		return nil, "", false
	}
	if b, isBool := raw.(bool); !isBool || !b {
		return nil, fmt.Sprintf("@%s(public) takes no value -- write @%s(public, %s)",
			RowAuthzAnnotation, RowAuthzAnnotation, rowAuthzArgRequiresIdentity), false
	}
	decl := &RowAuthzDecl{Tier: RowAuthzPublic}
	for name, value := range args {
		if name == "public" {
			continue
		}
		// A SECOND TIER is not an unknown modifier, and must not be
		// reported as one. `@rowAuthz(public, clusterOwner)` and
		// `@rowAuthz(owner="x", public)` are ambiguous DECLARATIONS -- two
		// floors in one list -- and the parser's standing rule is that it
		// never resolves that by picking a side. Declining the shape here
		// (rather than refusing it) is what routes them to the shared
		// "takes exactly one tier" diagnostic, which is the sentence that
		// actually describes what is wrong with them.
		if _, isFlagTier := rowAuthzFlagTiers[name]; isFlagTier {
			return nil, "", false
		}
		if _, isKeywordTier := rowAuthzKeywordTiers[name]; isKeywordTier {
			return nil, "", false
		}
		bare, known := rowAuthzPublicModifiers[name]
		if !known {
			return nil, fmt.Sprintf("@%s(public, %s) is not a declaration this parser reads -- the public tier takes %s and nothing else",
				RowAuthzAnnotation, name, rowAuthzArgRequiresIdentity), false
		}
		b, isBool := value.(bool)
		if bare && (!isBool || !b) {
			return nil, fmt.Sprintf("@%s(public, %s) takes no value -- write it bare",
				RowAuthzAnnotation, name), false
		}
		if name == rowAuthzArgRequiresIdentity {
			decl.RequiresIdentity = true
		}
	}
	return decl, "", true
}

// parseRowAuthzOwnedModifiers reads the owned tier carrying one or more
// widening modifiers:
//
//	@rowAuthz(owner="<field>", clusterOwner)                  memql#4312
//	@rowAuthz(owner="<field>", rankVisible)                   memql#4834 (D2)
//	@rowAuthz(owner="<field>", rankVisible, rankStrict)       memql#4834 (D3)
//	@rowAuthz(owner="<field>", rankVisible, unowned="developer")
//
// It is the ONLY accepted argument list with more than one entry, and
// every accepted shape names ONE tier -- the owned tier -- plus
// arguments that widen it. Two TIERS in one list remains what it always
// was: not a wider tier but an ambiguous declaration, and an ambiguous
// authorization statement is the one thing this parser must never
// resolve by picking a side.
//
// Returns (nil, "", false) when the args are not that shape, so the
// caller emits the shared "takes exactly one tier" diagnostic. Returns
// (nil, reason, false) when the shape IS owned-with-modifiers but the
// combination is refused, so the caller can say WHICH argument is wrong
// rather than listing every accepted form at someone who is one word
// away from a legal declaration.
func parseRowAuthzOwnedModifiers(args map[string]any) (*RowAuthzDecl, string, bool) {
	rawOwner, hasOwner := args[rowAuthzArgOwner]
	if !hasOwner {
		return nil, "", false
	}
	owner, isString := rawOwner.(string)
	if !isString {
		return nil, "", false
	}
	if owner = strings.TrimSpace(owner); owner == "" {
		return nil, "", false
	}

	decl := &RowAuthzDecl{Tier: RowAuthzOwned, Owner: owner}
	for name, raw := range args {
		if name == rowAuthzArgOwner {
			continue
		}
		isFlag, known := rowAuthzOwnedModifiers[name]
		if !known {
			// Not a modifier at all -- fall back to the shared "takes
			// exactly one tier" diagnostic, which is the right message
			// for `@rowAuthz(owner="x", via="y")`.
			return nil, "", false
		}
		if isFlag {
			// A flag modifier must be BARE: the parser stores a bare
			// identifier as `true`, so `rankVisible="yes"` is a different
			// shape and is not a spelling of this modifier.
			if b, isBool := raw.(bool); !isBool || !b {
				return nil, fmt.Sprintf("@%s(%s) takes no value -- write it bare, as @%s(%s=%q, %s). Accepted: %s",
					RowAuthzAnnotation, name, RowAuthzAnnotation, rowAuthzArgOwner, owner, name, rowAuthzSpellings()), false
			}
			switch name {
			case rowAuthzArgClusterOwner:
				decl.ClusterOwnerBypass = true
			case rowAuthzArgRankVisible:
				decl.RankVisible = true
			case rowAuthzArgRankStrict:
				decl.RankStrict = true
			}
			continue
		}
		// A keyword modifier carries a quoted value.
		v, ok := raw.(string)
		if !ok || strings.TrimSpace(v) == "" {
			return nil, fmt.Sprintf("@%s(%s=...) requires a quoted role slug -- write @%s(%s=%q, %s=\"developer\"). Accepted: %s",
				RowAuthzAnnotation, name, RowAuthzAnnotation, rowAuthzArgOwner, owner, name, rowAuthzSpellings()), false
		}
		if name == rowAuthzArgUnowned {
			decl.Unowned = strings.TrimSpace(v)
		}
	}

	// The one incoherent pairing, refused rather than resolved: a write
	// rule with no matching read rule grants the authority to change a row
	// the same caller cannot see. Whichever way an engine resolved that it
	// would be surprising, so it is not resolved.
	if decl.RankStrict && !decl.RankVisible {
		return nil, fmt.Sprintf("@%s(%s) needs %s beside it -- rank-strict writes let a caller change rows that rank-visible reads are what let them SEE. Write @%s(%s=%q, %s, %s). Accepted: %s",
			RowAuthzAnnotation, rowAuthzArgRankStrict, rowAuthzArgRankVisible,
			RowAuthzAnnotation, rowAuthzArgOwner, owner, rowAuthzArgRankVisible, rowAuthzArgRankStrict, rowAuthzSpellings()), false
	}
	// `unowned` is a rank floor and rank floors are meaningless without
	// the rank branch: the unowned row would still be reachable through
	// the cluster-owner escape alone, so the declaration would read as a
	// widening that does nothing.
	if decl.Unowned != "" && !decl.RankVisible {
		return nil, fmt.Sprintf("@%s(%s=%q) needs %s beside it -- without the rank branch an unowned row is still reachable through %s alone, so the floor would gate nothing. Write @%s(%s=%q, %s, %s=%q). Accepted: %s",
			RowAuthzAnnotation, rowAuthzArgUnowned, decl.Unowned, rowAuthzArgRankVisible, rowAuthzArgClusterOwner,
			RowAuthzAnnotation, rowAuthzArgOwner, owner, rowAuthzArgRankVisible, rowAuthzArgUnowned, decl.Unowned, rowAuthzSpellings()), false
	}
	return decl, "", true
}

// ParseRowAuthz is THE detector: it turns an `@rowAuthz(...)`
// attribute into its declared meaning, or into the diagnostic
// explaining why it has none. Both the loader and the codemod read
// declarations through this function and nothing else.
//
// The returned error never names the concept -- the caller holds that
// and wraps, the way applyConceptAttribute's siblings do.
func ParseRowAuthz(attr *Attribute) (*RowAuthzDecl, error) {
	if attr == nil {
		return nil, fmt.Errorf("@%s: nil attribute", RowAuthzAnnotation)
	}
	if attr.Name != RowAuthzAnnotation {
		return nil, fmt.Errorf("@%s: called on @%s", RowAuthzAnnotation, attr.Name)
	}

	// `@rowAuthz("public")` -- the single-string form. Rejected rather
	// than accepted as a synonym: one spelling per tier is what makes
	// the declaration greppable, and a second spelling is drift
	// surface with no upside.
	if attr.Value != nil {
		return nil, fmt.Errorf("@%s does not take a bare value -- write one of: %s",
			RowAuthzAnnotation, rowAuthzSpellings())
	}
	if len(attr.Args) == 0 {
		return nil, fmt.Errorf("@%s requires a tier -- write one of: %s",
			RowAuthzAnnotation, rowAuthzSpellings())
	}
	if len(attr.Args) > 1 {
		// The one legal multi-argument list: the OWNED tier carrying
		// widening modifiers -- the composite (memql#4312) and the rank
		// modifiers (memql#4834). "The owner, or a cluster owner" and
		// "the owner, or anyone above the owner's rank" are each a single
		// floor -- the owned tier with a gate ORed in, not two tiers --
		// so neither violates the rule the message below states.
		decl, refusal, ok := parseRowAuthzOwnedModifiers(attr.Args)
		if ok {
			return decl, nil
		}
		// The second legal multi-argument list: the PUBLIC tier carrying
		// its narrowing modifier (memql#4809). Tried after the owned one
		// because the owned shapes are the overwhelming majority and this
		// keeps their diagnostics first; the two are disjoint (one needs
		// `owner=`, the other needs `public`), so the order is a matter of
		// which refusal an author sees, never of which decl they get.
		if publicDecl, publicRefusal, publicOk := parseRowAuthzPublicModifiers(attr.Args); publicOk {
			return publicDecl, nil
		} else if publicRefusal != "" {
			return nil, fmt.Errorf("%s", publicRefusal)
		}
		// A refusal means the shape WAS owned-with-modifiers and one
		// argument is wrong. Saying which beats listing every accepted
		// form at an author who is one word away from a legal
		// declaration.
		if refusal != "" {
			return nil, fmt.Errorf("%s", refusal)
		}
		return nil, fmt.Errorf("@%s takes exactly one tier, got %d (%s) -- a concept declares a single floor. Write one of: %s",
			RowAuthzAnnotation, len(attr.Args), strings.Join(sortedArgNames(attr.Args), ", "), rowAuthzSpellings())
	}

	// Exactly one entry; pull it out.
	var name string
	var raw any
	for k, v := range attr.Args {
		name, raw = k, v
	}

	if tier, ok := rowAuthzFlagTiers[name]; ok {
		// A flag tier carries no value: the parser stores a bare
		// identifier as `true`. `@rowAuthz(public="x")` is a
		// different shape and is not a spelling of this tier.
		if b, isBool := raw.(bool); !isBool || !b {
			return nil, fmt.Errorf("@%s(%s) takes no value -- write @%s(%s)",
				RowAuthzAnnotation, name, RowAuthzAnnotation, name)
		}
		return &RowAuthzDecl{Tier: tier}, nil
	}

	if tier, ok := rowAuthzKeywordTiers[name]; ok {
		s, isString := raw.(string)
		if !isString {
			return nil, fmt.Errorf("@%s(%s=...) requires a quoted %s -- write @%s(%s=%q)",
				RowAuthzAnnotation, name, rowAuthzValueNoun(name), RowAuthzAnnotation, name, rowAuthzValuePlaceholder(name))
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("@%s(%s=\"\") is empty -- name the %s the tier gates on",
				RowAuthzAnnotation, name, rowAuthzValueNoun(name))
		}
		decl := &RowAuthzDecl{Tier: tier}
		if tier == RowAuthzOwned {
			decl.Owner = s
		} else {
			decl.Spec = s
		}
		return decl, nil
	}

	return nil, fmt.Errorf("@%s: unknown tier %q -- write one of: %s",
		RowAuthzAnnotation, name, rowAuthzSpellings())
}

func rowAuthzValueNoun(arg string) string {
	if arg == rowAuthzArgOwner {
		return "field name"
	}
	return "spec name"
}

func rowAuthzValuePlaceholder(arg string) string {
	if arg == rowAuthzArgOwner {
		return "ownerUserId"
	}
	return "spaceMember"
}

// sortedArgNames renders an attribute's argument names in a stable
// order for a diagnostic. (The package's existing sortedKeys takes a
// membership set, not an arg map.)
func sortedArgNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FormatRowAuthz renders a declaration in its canonical spelling. The
// codemod emits through this and only this, so anything it can write
// is by construction something ParseRowAuthz reads back to the same
// decl -- the round-trip the loader/codemod agreement test asserts.
func FormatRowAuthz(d RowAuthzDecl) (string, error) {
	// ClusterOwnerBypass widens the OWNED tier and means nothing on any
	// other. Refused rather than dropped: a renderer that silently
	// discards half of a declaration emits something ParseRowAuthz reads
	// back as a DIFFERENT decl, which is precisely the round-trip this
	// function exists to guarantee.
	if d.ClusterOwnerBypass && d.Tier != RowAuthzOwned {
		return "", fmt.Errorf("@%s: the clusterOwner bypass is an argument of the owned tier -- it has no meaning on tier %q; write @%s(%s=\"<field>\", %s)",
			RowAuthzAnnotation, d.Tier, RowAuthzAnnotation, rowAuthzArgOwner, rowAuthzArgClusterOwner)
	}
	// Same rule for the rank modifiers, and for the same reason: a
	// renderer that silently discards half a declaration emits something
	// ParseRowAuthz reads back as a DIFFERENT decl.
	for _, m := range []struct {
		set  bool
		name string
	}{
		{d.RankVisible, rowAuthzArgRankVisible},
		{d.RankStrict, rowAuthzArgRankStrict},
		{d.Unowned != "", rowAuthzArgUnowned},
	} {
		if m.set && d.Tier != RowAuthzOwned {
			return "", fmt.Errorf("@%s: %s is an argument of the owned tier -- it has no meaning on tier %q",
				RowAuthzAnnotation, m.name, d.Tier)
		}
	}
	// RequiresIdentity narrows the PUBLIC tier and means nothing on any
	// other, and is refused rather than dropped for the same round-trip
	// reason its widening siblings are.
	if d.RequiresIdentity && d.Tier != RowAuthzPublic {
		return "", fmt.Errorf("@%s: %s is an argument of the public tier -- it has no meaning on tier %q",
			RowAuthzAnnotation, rowAuthzArgRequiresIdentity, d.Tier)
	}
	if d.RankStrict && !d.RankVisible {
		return "", fmt.Errorf("@%s: %s without %s is not a declaration this parser reads back -- see ParseRowAuthz",
			RowAuthzAnnotation, rowAuthzArgRankStrict, rowAuthzArgRankVisible)
	}
	if d.Unowned != "" && !d.RankVisible {
		return "", fmt.Errorf("@%s: %s without %s is not a declaration this parser reads back -- see ParseRowAuthz",
			RowAuthzAnnotation, rowAuthzArgUnowned, rowAuthzArgRankVisible)
	}
	switch d.Tier {
	case RowAuthzPublic:
		if d.RequiresIdentity {
			return fmt.Sprintf("@%s(%s, %s)", RowAuthzAnnotation, d.Tier, rowAuthzArgRequiresIdentity), nil
		}
		return fmt.Sprintf("@%s(%s)", RowAuthzAnnotation, d.Tier), nil
	case RowAuthzClusterOwner:
		return fmt.Sprintf("@%s(%s)", RowAuthzAnnotation, d.Tier), nil
	case RowAuthzOwned:
		if strings.TrimSpace(d.Owner) == "" {
			return "", fmt.Errorf("@%s: tier %q needs an owner field", RowAuthzAnnotation, d.Tier)
		}
		// Canonical modifier ORDER, so the round-trip is byte-stable:
		// the read widenings first (rankVisible, then the write rule it
		// gates, then the unowned floor it makes meaningful), and the
		// cluster-owner escape last because it is the oldest and every
		// existing declaration in the tree already spells it there.
		out := fmt.Sprintf("@%s(%s=%q", RowAuthzAnnotation, rowAuthzArgOwner, d.Owner)
		if d.RankVisible {
			out += ", " + rowAuthzArgRankVisible
		}
		if d.RankStrict {
			out += ", " + rowAuthzArgRankStrict
		}
		if d.Unowned != "" {
			out += fmt.Sprintf(", %s=%q", rowAuthzArgUnowned, d.Unowned)
		}
		if d.ClusterOwnerBypass {
			out += ", " + rowAuthzArgClusterOwner
		}
		return out + ")", nil
	case RowAuthzGranted:
		if strings.TrimSpace(d.Spec) == "" {
			return "", fmt.Errorf("@%s: tier %q needs a spec name", RowAuthzAnnotation, d.Tier)
		}
		return fmt.Sprintf("@%s(%s=%q)", RowAuthzAnnotation, rowAuthzArgVia, d.Spec), nil
	default:
		return "", fmt.Errorf("@%s: unknown tier %q", RowAuthzAnnotation, d.Tier)
	}
}

// rowAuthzDeclPattern matches an `@rowAuthz` annotation.
//
// NOT line-anchored, unlike the sibling actor-binding detector. The
// parser accepts several annotations on one line, so
// `@description("d") @rowAuthz(public)` is a real declaration that a
// `^[ \t]*@rowAuthz` pattern does not see -- and not seeing it made
// the codemod insert a SECOND declaration above a concept that already
// had one, silently replacing a hand-authored tier. False positives
// from prose are handled by blanking rather than by anchoring, which
// is the stronger guard: `@` cannot appear inside an identifier, so
// there is nothing else for `@rowAuthz` to be.
var rowAuthzDeclPattern = regexp.MustCompile(`@` + RowAuthzAnnotation + `\b`)

// RowAuthzDeclaredInSource reports whether the source carries an
// @rowAuthz annotation. Used for the codemod's idempotency check -- a
// concept that already declares a tier is never rewritten, so a re-run
// is a no-op and a hand-authored declaration is never clobbered.
//
// Scanned on the comment- and string-blanked view so a doc comment
// that MENTIONS the annotation does not read as a declaration.
func RowAuthzDeclaredInSource(source string) bool {
	return rowAuthzDeclPattern.MatchString(blankCommentsAndStrings(source))
}

// conceptHeaderRe matches a concept declaration line. Group 1 is the
// indentation, group 2 the declared name.
var conceptHeaderRe = regexp.MustCompile(`(?m)^([ \t]*)concept[ \t]+([\p{L}_][` + identChar + `]*)`)

// ConceptHeader is one concept declaration located in a source file.
type ConceptHeader struct {
	Name string // the declared name, e.g. "plan"
	// Start is the byte offset of the header line's first character.
	Start int
	// End is the exclusive byte offset of the construct's closing
	// brace.
	End int
	// PreambleStart is the byte offset where the contiguous run of
	// annotation and comment lines above the header begins.
	PreambleStart int
	// Indent is the header's leading whitespace.
	Indent string
}

// ConceptHeaders locates every concept declaration in src.
//
// Headers are matched on the COMMENT- AND STRING-BLANKED view, not on
// raw source, so the word "concept" inside a doc comment or a
// @description string can never be mistaken for a declaration. The
// blanker is length-preserving, so every offset it yields indexes the
// raw text unchanged -- which is what keeps this off the class of bug
// where a rewriter locates on one view and slices on another (#2948).
func ConceptHeaders(src string) []ConceptHeader {
	blanked := blankCommentsAndStrings(src)
	matches := conceptHeaderRe.FindAllStringSubmatchIndex(blanked, -1)
	out := make([]ConceptHeader, 0, len(matches))
	for _, m := range matches {
		start := m[0]
		out = append(out, ConceptHeader{
			Name:          blanked[m[4]:m[5]],
			Start:         start,
			End:           conceptEnd(blanked, start),
			PreambleStart: preambleStart(src, start),
			Indent:        blanked[m[2]:m[3]],
		})
	}
	return out
}

// conceptEnd returns the exclusive end offset of the concept whose
// header starts at headerStart: the matching close of its body brace.
// Operates on the already-blanked view, so brace counting cannot be
// thrown by a brace inside a string or a comment.
func conceptEnd(blanked string, headerStart int) int {
	braceIdx := strings.IndexByte(blanked[headerStart:], '{')
	if braceIdx < 0 {
		return len(blanked)
	}
	depth := 0
	for i := headerStart + braceIdx; i < len(blanked); i++ {
		switch blanked[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(blanked)
}

// RewriteRowAuthz inserts a `@rowAuthz(...)` line above every concept
// named in tiers whose preamble does not already declare one. It is
// the codemod half of #2920, and it is re-runnable: a concept that
// already carries a declaration -- inferred on an earlier run or
// written by hand -- is left exactly as it was.
//
// Concepts absent from tiers are untouched, which is how "the
// inference had no evidence for this one" stays visible as an
// undeclared concept rather than being papered over with a guess.
func RewriteRowAuthz(src []byte, tiers map[string]RowAuthzDecl) ([]byte, error) {
	if len(tiers) == 0 {
		return src, nil
	}
	text := string(src)
	headers := ConceptHeaders(text)
	if len(headers) == 0 {
		return src, nil
	}
	var b strings.Builder
	prev := 0
	changed := false
	// prevEnd is where the previous concept's body closed. Everything
	// from there to this concept's header belongs to THIS concept --
	// annotations, comments, and blank lines between them.
	prevEnd := 0
	for _, h := range headers {
		// Header STARTS are monotonic (the regex scans forward), but
		// header ENDS are not: a spurious header -- a field named
		// `concept`, or a real one in a file whose braces do not
		// balance -- closes on the first `{...}` after it, which can
		// be before the enclosing construct's close. So `prevEnd` can
		// overshoot this header, and `text[prevEnd:h.End]` would slice
		// backwards and panic on input this function is supposed to
		// merely decline to rewrite.
		//
		// Clamping to the preamble bound restores the invariant
		// (PreambleStart <= Start <= End) and does the right thing for
		// the silent-corruption variant too: when a spurious header
		// pushes prevEnd past a REAL following concept, that concept's
		// region would otherwise be empty, its existing declaration
		// invisible, and a duplicate inserted.
		regionStart := prevEnd
		if regionStart > h.Start {
			regionStart = h.PreambleStart
		}
		regionEnd := h.End
		if regionEnd < h.Start {
			regionEnd = h.Start
		}
		region := text[regionStart:regionEnd]
		if h.End > prevEnd {
			prevEnd = h.End
		}

		decl, want := tiers[h.Name]
		if !want {
			continue
		}
		// A declaration anywhere in that region means this concept is
		// already spoken for.
		//
		// The region runs from the previous concept's closing brace,
		// NOT from h.PreambleStart. preambleStart stops at the first
		// blank line, and the parser happily attaches an annotation
		// separated from its header by one:
		//
		//	@rowAuthz(public)
		//
		//	concept thing { ... }
		//
		// loads as a real `public` declaration. Bounding at the
		// preamble made that invisible, so the codemod inserted a
		// SECOND declaration -- and since a duplicate is a load error,
		// a re-run turned a loadable tree into one that refuses to
		// boot. Everything between two concepts is whitespace,
		// comments or annotations belonging to the following one, so
		// the wider region cannot pick up a neighbour's declaration.
		if RowAuthzDeclaredInSource(region) {
			continue
		}
		line, err := FormatRowAuthz(decl)
		if err != nil {
			return nil, fmt.Errorf("concept %s: %w", h.Name, err)
		}
		b.WriteString(text[prev:h.Start])
		b.WriteString(h.Indent)
		b.WriteString(line)
		b.WriteString("\n")
		prev = h.Start
		changed = true
	}
	if !changed {
		return src, nil
	}
	b.WriteString(text[prev:])
	return []byte(b.String()), nil
}

// BlankCommentsAndStrings is the exported form of the blanker, for the
// memqlmigrate codemod, which has to scan construct bodies in the same
// coordinate space this package's header location uses.
func BlankCommentsAndStrings(source string) string {
	return blankCommentsAndStrings(source)
}

// blankCommentsAndStrings replaces the contents of line comments,
// block comments and string literals with spaces, preserving every
// byte offset and every newline so offsets into the result index the
// original unchanged.
//
// It is a strict superset of actor_binding.go's stripCommentsAndStrings,
// which predates block-comment support in the lexer and therefore does
// not blank `/* ... */`. This one does, and it consumes backslash
// escapes inside strings so `"he said \"concept\""` cannot end the
// string early. Kept local rather than widening the actor-binding
// helper: that one backs a shipped load rule whose behaviour is
// asserted by its own tests, and changing what it considers a comment
// is a separate change from adding this annotation.
//
// # Where a string ends, and why that is the lexer's answer
//
// A string literal ends at its closing quote and NOWHERE ELSE: it may
// span newlines, and an unterminated one runs to EOF. That is the
// lexer's reading (scanString accepts an interior newline as an
// ordinary rune) rather than this scanner's own. The one place the two
// cannot agree is an unterminated literal, because the lexer's answer
// there is a hard error -- "unterminated string starting at position
// %d" -- and a blanker has to return a string. Running to EOF is the
// compatible choice: it never exposes string content as code, so a
// caller handed a file that does not lex gets a conservative view
// instead of a scrambled one. There is nothing else to be tolerant of,
// since no consumer meaningfully runs on a file the lexer refuses.
//
// This replaced a line-bounded rule that claimed to mirror "the same
// recovery the lexer performs" (memql#3116). The lexer performs no such
// recovery, and the claim is what stopped readers checking. A multi-line
// literal desynced the scan in BOTH directions at once -- content after
// the newline surfaced as code, and the literal's real closing quote
// opened a string that swallowed the code after it -- until the next
// quote resynchronised.
//
// # What that cost each consumer (memql#3116 assessment)
//
//   - ConceptHeaders: `concept phantom {` written inside a wrapped
//     @description was located as a real declaration, and the codemod
//     inserts relative to a header's PreambleStart -- an insertion
//     INSIDE a string literal. The same desync unbalanced conceptEnd's
//     brace walk, so a real concept's body could end early or run into
//     its neighbour.
//   - RowAuthzDeclaredInSource: a declaration sharing a line with a
//     multi-line literal's closing quote (`...wraps") @rowAuthz(public)`,
//     the multi-annotation line rowAuthzDeclPattern is deliberately not
//     anchored for) was swallowed as string content. The idempotency
//     check then said "undeclared" and RewriteRowAuthz inserted a
//     duplicate -- a load error, so a re-run broke a loadable tree.
//   - BlankCommentsAndStrings: memqlmigrate's inference slices construct
//     bodies by walking braces over this view, so a `}` on a literal's
//     second line closed a body early and hid the filter inside it. A
//     construct whose filter is invisible reads as unfiltered, which
//     BLOCKS -- a correct declaration silently dropped.
//
// Each of those three is pinned by a test.
func blankCommentsAndStrings(source string) string {
	var b strings.Builder
	b.Grow(len(source))
	const (
		code = iota
		inLineComment
		inBlockComment
		inStr
	)
	state := code
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch state {
		case inLineComment:
			if c == '\n' {
				state = code
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case inBlockComment:
			if c == '*' && i+1 < len(source) && source[i+1] == '/' {
				b.WriteString("  ")
				i++
				state = code
				continue
			}
			if c == '\n' {
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case inStr:
			switch {
			case c == '\\' && i+1 < len(source) && source[i+1] != '\n':
				// Blank the escape and the byte it escapes together,
				// so `\"` cannot be read as a closing quote.
				//
				// A backslash immediately before a newline is excluded
				// so the newline reaches the arm below and survives as
				// a newline. The lexer rejects `\<newline>` as an
				// invalid escape, so this shape only ever occurs in a
				// file that does not lex; the offset- and
				// line-preserving contract still has to hold on it.
				b.WriteString("  ")
				i++
			case c == '"':
				state = code
				b.WriteByte(c)
			case c == '\n':
				// A newline does NOT end the literal: it is written
				// through as ordinary content and the scan stays in
				// string state (memql#3116).
				//
				// This is what the lexer does. scanString writes a
				// literal newline into the builder as an ordinary rune
				// and keeps going, so a multi-line literal is ONE
				// string token -- memql#3047's line counter exists
				// precisely because such literals are legal. Ending
				// here instead desynced the scan in both directions at
				// once: content after the newline was exposed as code,
				// and the literal's real closing quote OPENED a string
				// that swallowed the code following it.
				//
				// The newline is emitted rather than blanked so the
				// result stays line-for-line and byte-for-byte
				// alignable with the source.
				b.WriteByte(c)
			default:
				b.WriteByte(' ')
			}
		default:
			switch {
			case c == '"':
				state = inStr
				b.WriteByte(c)
			case c == '/' && i+1 < len(source) && source[i+1] == '/':
				state = inLineComment
				b.WriteByte(' ')
			case c == '/' && i+1 < len(source) && source[i+1] == '*':
				b.WriteString("  ")
				i++
				state = inBlockComment
			default:
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
