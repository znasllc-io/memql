package parser

// @belongsTo -- which ACCOUNT a concept's rows belong to, or that they belong to
// none (design record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md).
//
//	@belongsTo(account)                    its rows belong to an account
//	@belongsTo(account, via="goalId")      ... the account of the row `goalId` names
//	@belongsTo(account, via="storeId", parent="v1:shopify:store")
//	                                       ... the same, where the field carries no
//	                                       @relationship to say which concept it names
//	@belongsTo(account, shared)            ... and a row with no account is provided
//	                                       by the cluster to every account
//	@belongsTo(account, stated)            ... and the concept's own writers state
//	                                       which; the engine leaves the field alone
//	@belongsTo(account, stays="signin")    ... and its rows never leave this
//	                                       cluster: an account export counts them
//	                                       and carries none (any account form)
//	@belongsTo(cluster)                    cluster-wide: never inside an account
//
// WHAT IT IS NOT. It does not open a row to anybody. `@rowAuthz(account=...)` is
// the declaration that shares a row with an account's members; this one only
// RECORDS which account a row belongs to. A note records its account and stays
// private to its author. The two are separate on purpose: every reader of "whose
// row is this" (the engine's stamp, the assignment of existing rows, the account
// inventory, and later export and sign-in) asks this declaration, and none of
// them should have to reason about who may read the row to do it.
//
// THE FIELD IS ALWAYS `accountId`. One name, so a reader needs no per-concept
// knowledge. The declaration says HOW the field gets its value, never where it
// lives.
//
// This file owns the SHAPE of a declaration: which combinations of keys mean
// something. Which keys exist at all is the annotation registry's
// (component/language/annotations); whether the named fields exist is the concept
// translator's, which is the first place the property list is known; and whether
// a `via` chain is sound across concepts is the engine's load, which is the first
// place every concept is known.

import (
	"fmt"
	"sort"
	"strings"
)

// BelongsToAnnotation is the annotation's name, without the `@`.
const BelongsToAnnotation = "belongsTo"

// BelongsToAccountField is the payload field an account-scoped concept records
// its account in. Fixed, never configurable: see the file header.
const BelongsToAccountField = "accountId"

// BelongsToScope is what a concept's rows belong to.
type BelongsToScope string

const (
	// BelongsToAccount: every row belongs to one account (or, for a shared
	// concept, to none and is then cluster-provided).
	BelongsToAccount BelongsToScope = "account"
	// BelongsToCluster: the concept is cluster-wide. Its rows are never inside
	// an account, and nothing stamps or reads an account on them.
	BelongsToCluster BelongsToScope = "cluster"
)

// The stable codes a @belongsTo refusal carries. Conflict, field, via-unknown,
// parent-unknown, stays-unknown and sharing-mismatch are raised while one
// concept is built; via-cluster, via-cycle and via-stays need every concept and
// are raised by the engine's load.
const (
	CodeBelongsToConflict        = "belongs_to_conflict"
	CodeBelongsToField           = "belongs_to_field"
	CodeBelongsToViaUnknown      = "belongs_to_via_unknown"
	CodeBelongsToParentUnknown   = "belongs_to_parent_unknown"
	CodeBelongsToViaCluster      = "belongs_to_via_cluster"
	CodeBelongsToViaCycle        = "belongs_to_via_cycle"
	CodeBelongsToSharingMismatch = "belongs_to_sharing_mismatch"
	CodeBelongsToStaysUnknown    = "belongs_to_stays_unknown"
	CodeBelongsToViaStays        = "belongs_to_via_stays"
	// CodeBelongsToUndeclared is the load's refusal of a concept that declares
	// no @belongsTo, once that form's deprecation window has closed
	// (component/language/deprecation, ConceptBelongsToUndeclared), and the
	// promotion's refusal of an authored concept with none.
	CodeBelongsToUndeclared = "belongs_to_undeclared"
)

// The reasons a kind STAYS on its cluster: an account export counts its rows,
// prints the reason in the file's report, and carries none of them (design
// record docs/superpowers/specs/2026-11-12-account-export-design.md, section C).
// A closed list, because the report and an import read the reason: an open
// value would let a typo stand as a fourth reason nobody explains.
const (
	// BelongsToStaysSignin: sign-in material or a visit (an access session, a
	// preview grant, an invitation).
	BelongsToStaysSignin = "signin"
	// BelongsToStaysTransit: work in transit that means nothing elsewhere (an
	// upload session, an outbox entry, a send in flight).
	BelongsToStaysTransit = "transit"
	// BelongsToStaysMachine: bound to a machine of this cluster (a workbench
	// workspace, a watched folder).
	BelongsToStaysMachine = "machine"
)

// BelongsToStaysReasons returns the three reasons, in the order the report and
// the documentation print them. A fresh slice each call.
func BelongsToStaysReasons() []string {
	return []string{BelongsToStaysSignin, BelongsToStaysTransit, BelongsToStaysMachine}
}

// belongsToStaysList is the three reasons as a refusal prints them.
const belongsToStaysList = BelongsToStaysSignin + ", " + BelongsToStaysTransit + " or " + BelongsToStaysMachine

func isBelongsToStaysReason(s string) bool {
	for _, r := range BelongsToStaysReasons() {
		if s == r {
			return true
		}
	}
	return false
}

// BelongsToDecl is one concept's declaration. A nil *BelongsToDecl means the
// concept declared nothing, which is a different statement from cluster-wide:
// the engine's own tree may not leave it out, and a mounted product bundle that
// does is reported.
type BelongsToDecl struct {
	Scope BelongsToScope `json:"scope"`
	// Via is the payload field naming the PARENT row whose account this row
	// takes. Empty for a root.
	Via string `json:"via,omitempty"`
	// Parent is the concept id the Via field names, written out only when the
	// field carries no @relationship to say so and stores a bare id. Empty
	// otherwise: a field with a relationship names its target there, and a
	// field holding canonical ids of several concepts (an index row's
	// reference) is resolved from the id itself.
	Parent string `json:"parent,omitempty"`
	// Shared marks a concept that holds cluster-provided rows beside account
	// rows: a seeded skill beside one a person wrote, a cluster-wide role
	// beside a scoped one.
	Shared bool `json:"shared,omitempty"`
	// Stated marks a concept whose OWN WRITERS state the account: its
	// mutations accept or copy `accountId`, and rules that predate this
	// declaration decide what an absent value means and whether it may change
	// (a role with no account is a cluster-wide role; a campaign moves between
	// accounts through the organization transfer). The engine never fills the
	// field on such a concept, never refuses a change to it, and the
	// assignment of existing rows leaves it alone. Without it the account is
	// ENGINE-MANAGED: filled at write time and fixed from then on.
	Stated bool `json:"stated,omitempty"`
	// Stays is the reason the concept's rows never leave this cluster, one of
	// BelongsToStaysReasons, or empty for a kind that travels. It changes
	// nothing about which account a row belongs to: the row is the account's
	// all the same, and only an account export reads it.
	Stays string `json:"stays,omitempty"`
}

// AccountScoped reports whether the declaration places rows in accounts.
func (d *BelongsToDecl) AccountScoped() bool {
	return d != nil && d.Scope == BelongsToAccount
}

// EngineManaged reports whether the ENGINE decides the row's account: the
// concept is account-scoped and does not leave the field to its own writers.
// It is the one question the stamp and the assignment of existing rows ask.
func (d *BelongsToDecl) EngineManaged() bool {
	return d.AccountScoped() && !d.Stated
}

// BelongsToError is a refusal of a @belongsTo declaration. It carries a stable
// code (baseloader's CodedRefusal reads RuleCode) and ends its text with the
// code in brackets, the way every coded load refusal does.
type BelongsToError struct {
	Code string
	Msg  string
}

func (e *BelongsToError) Error() string    { return e.Msg + " [" + e.Code + "]" }
func (e *BelongsToError) RuleCode() string { return e.Code }

// NewBelongsToError builds a coded refusal. Exported for the two later checks
// that live outside this package.
func NewBelongsToError(code, format string, args ...any) *BelongsToError {
	return &BelongsToError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

const belongsToSpellings = "@belongsTo(account), @belongsTo(account, via=\"<field>\"), " +
	"@belongsTo(account, shared), @belongsTo(account, stated) or @belongsTo(cluster)"

// ParseBelongsTo turns a `@belongsTo(...)` attribute into the declaration it
// makes, or into the refusal explaining why it makes none.
//
// The registry has already held the attribute to its keys and their shapes, so
// what is decided here is which combinations mean something. The returned error
// never names the concept; the caller holds that and wraps.
func ParseBelongsTo(attr *Attribute) (*BelongsToDecl, error) {
	if attr == nil {
		return nil, fmt.Errorf("@%s: nil attribute", BelongsToAnnotation)
	}
	if attr.Name != BelongsToAnnotation {
		return nil, fmt.Errorf("@%s: called on @%s", BelongsToAnnotation, attr.Name)
	}
	if attr.Value != nil || len(attr.Args) == 0 {
		return nil, NewBelongsToError(CodeBelongsToConflict,
			"@%s says what a concept's rows belong to -- write %s", BelongsToAnnotation, belongsToSpellings)
	}

	account := belongsToFlag(attr.Args, "account")
	cluster := belongsToFlag(attr.Args, "cluster")
	shared := belongsToFlag(attr.Args, "shared")
	stated := belongsToFlag(attr.Args, "stated")
	via, hasVia := belongsToString(attr.Args, "via")
	parent, hasParent := belongsToString(attr.Args, "parent")
	stays, hasStays := belongsToString(attr.Args, "stays")

	switch {
	case account && cluster:
		return nil, NewBelongsToError(CodeBelongsToConflict,
			"@%s(account, cluster) names both -- a concept's rows belong to an account or are cluster-wide, never both. Write %s",
			BelongsToAnnotation, belongsToSpellings)
	case !account && !cluster:
		return nil, NewBelongsToError(CodeBelongsToConflict,
			"@%s(%s) names neither `account` nor `cluster` -- write %s",
			BelongsToAnnotation, strings.Join(belongsToArgNames(attr.Args), ", "), belongsToSpellings)
	}

	if cluster {
		if shared || stated || hasVia || hasParent || hasStays {
			return nil, NewBelongsToError(CodeBelongsToConflict,
				"@%s(cluster) takes nothing else -- `shared`, `stated`, `via`, `parent` and `stays` describe rows that belong to an account, and a cluster-wide concept has none",
				BelongsToAnnotation)
		}
		return &BelongsToDecl{Scope: BelongsToCluster}, nil
	}

	if stated && hasVia {
		return nil, NewBelongsToError(CodeBelongsToConflict,
			"@%s(account, stated, via=%q) names both -- `stated` leaves the account to the concept's own writers, and `via` has the engine take it from a parent. Write one",
			BelongsToAnnotation, via)
	}

	if hasVia && via == "" {
		return nil, NewBelongsToError(CodeBelongsToViaUnknown,
			"@%s(account, via=\"\") names no field -- `via` is the payload field holding the id of the row whose account this row takes",
			BelongsToAnnotation)
	}
	if hasParent && !hasVia {
		return nil, NewBelongsToError(CodeBelongsToConflict,
			"@%s(account, parent=%q) has no `via` -- `parent` names the concept a `via` field points at, so it is written only beside one",
			BelongsToAnnotation, parent)
	}
	if hasParent && parent == "" {
		return nil, NewBelongsToError(CodeBelongsToParentUnknown,
			"@%s(account, via=%q, parent=\"\") names no concept -- write the concept id the field holds, or drop `parent` when the field carries a @relationship",
			BelongsToAnnotation, via)
	}
	if shared && hasVia {
		return nil, NewBelongsToError(CodeBelongsToConflict,
			"@%s(account, shared, via=%q) names both -- a row that follows a parent is cluster-provided exactly when its parent is, so `shared` is declared on the parent concept only",
			BelongsToAnnotation, via)
	}

	if hasStays && !isBelongsToStaysReason(stays) {
		return nil, NewBelongsToError(CodeBelongsToStaysUnknown,
			"@%s(account, stays=%q) gives no reason the kind stays -- write stays= with one of %s, or drop `stays` so the kind travels in an account export",
			BelongsToAnnotation, stays, belongsToStaysList)
	}

	return &BelongsToDecl{Scope: BelongsToAccount, Via: via, Parent: parent, Shared: shared, Stated: stated, Stays: stays}, nil
}

// FormatBelongsTo renders a declaration as the annotation an author writes.
// It is the inverse of ParseBelongsTo for every declaration that function
// returns.
func FormatBelongsTo(d BelongsToDecl) string {
	if d.Scope == BelongsToCluster {
		return "@" + BelongsToAnnotation + "(cluster)"
	}
	parts := []string{"account"}
	if d.Shared {
		parts = append(parts, "shared")
	}
	if d.Stated {
		parts = append(parts, "stated")
	}
	if d.Via != "" {
		parts = append(parts, fmt.Sprintf("via=%q", d.Via))
	}
	if d.Parent != "" {
		parts = append(parts, fmt.Sprintf("parent=%q", d.Parent))
	}
	if d.Stays != "" {
		parts = append(parts, fmt.Sprintf("stays=%q", d.Stays))
	}
	return "@" + BelongsToAnnotation + "(" + strings.Join(parts, ", ") + ")"
}

// belongsToFlag reads a bare key. The parser stores a bare key as `true`, the
// same entry `key=true` makes; the registry has already refused a valued flag.
func belongsToFlag(args map[string]any, name string) bool {
	v, ok := args[name].(bool)
	return ok && v
}

// belongsToString reads a string key, trimmed, and whether it was written.
func belongsToString(args map[string]any, name string) (string, bool) {
	raw, ok := args[name]
	if !ok {
		return "", false
	}
	s, _ := raw.(string)
	return strings.TrimSpace(s), true
}

func belongsToArgNames(args map[string]any) []string {
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
