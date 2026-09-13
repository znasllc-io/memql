package auth

// THE ACTOR-SHAPED CAPABILITY RESOLVER (app access grants record, section 1;
// epic memql#5295).
//
// Capable(role, verb, resource) answered a question about a ROLE. This file
// answers the one every gate actually asks -- may THIS ACTOR do verb on
// resource -- and it is the only such function: the seven call sites that used
// to ask the role-shaped question ask this one now, and the role-shaped
// function is gone (memql#5296).
//
// # The rule
//
//  1. Start from the role catalog: does the actor's role hold (verb, resource),
//     with the catalog's own deny-wins rule inside that level.
//  2. Overlay the actor's active GROUP grants. Any active group grant on the
//     pair replaces the role's answer; if the actor's groups disagree, deny
//     wins within the level, since no group is more specific than another.
//  3. Overlay the actor's own USER grants. An active user grant replaces
//     whatever the previous levels said.
//
// Most specific wins ACROSS levels, deny wins WITHIN a level (decisions D1,
// D2). The alternative -- deny anywhere wins -- was rejected because it makes
// "give this one person access" fail silently whenever any group they are in
// says no.
//
// # Who is a subject
//
// A PRINCIPAL: a person holding a rung on the role ladder. MaintenanceActor,
// the seed materializer, an automation's system actor and borrowed authority
// (ContextWithUserActor) are not principals -- they carry
// AccessContext.Unranked, the rank rules do not govern them (memql#4832, D4),
// and neither do grants. A deny naming the person a worker borrows must not
// stop the worker's own write on that person's behalf. An Unranked subject
// resolves exactly as the role catalog alone answers, and costs no read.
//
// # Why the package holds the source rather than taking it as a parameter
//
// The same argument capability_catalog.go makes for the catalog: the gates are
// free functions called across four modules, and a half-threaded resolver --
// some sites reading rows, some not -- would have two opinions about one
// caller on one request. component/memql installs a graph-backed GrantSource
// at engine start and a MembershipSource beside it; before that, or on a node
// with no database, CapableFor is the catalog's answer and nothing else.
//
// # Cost (D9)
//
// Grants are read PER REQUEST and memoised on the context, beside the account
// scope. No new cache and no cross-node reload: a grant written on one replica
// is honoured on every other at the next request, because every request reads
// the rows. ContextWithGrantMemo installs the memo; component/memql does so at
// its execute entry, so two gates on one request cost one read of each level.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// The two subject kinds a grant may name. A ROLE IS NEVER A SUBJECT: a role's
// abilities are edited on the role, and putting them in two places is how
// they drift.
const (
	SubjectKindUser  = "user"
	SubjectKindGroup = "group"
)

// The two effects. Both are legal at every level (D1).
const (
	GrantAllow = "allow"
	GrantDeny  = "deny"
)

// Subject is the actor a capability question is asked about.
//
// Role and UserId come off the verified AccessContext; GroupIds are the
// actor's ACTIVE memberships, resolved by the installed MembershipSource and
// memoised by the engine per request. Unranked marks a non-principal, whose
// grants are not consulted -- see the header.
type Subject struct {
	Role     Role
	UserId   string
	GroupIds []string
	Unranked bool
}

// Grant is one v1:rbac:grant row as the resolver sees it: the pair it names
// and its effect. Inactive carries the row's lifecycle flag so that ONE place
// -- this resolver -- decides that an inactive grant decides nothing; a source
// reports the row and does not editorialise.
type Grant struct {
	Verb     string
	Resource string
	Effect   string
	Inactive bool
}

// GrantSource reads a subject's grants. Implemented by component/memql over
// the v1:rbac:grant rows (newest version per id), installed at engine start.
//
// Both methods are keyed by the subject's ids in whichever spelling the caller
// holds; the implementation matches bare and canonical alike, because an
// AccessContext.UserId arrives in both forms.
type GrantSource interface {
	// ActiveGrantsForUser returns the grants whose subject is this user.
	ActiveGrantsForUser(ctx context.Context, userId string) ([]Grant, error)
	// ActiveGrantsForGroups returns the grants whose subject is any of these
	// groups. Called only for a subject with at least one group.
	ActiveGrantsForGroups(ctx context.Context, groupIds []string) ([]Grant, error)
}

// MembershipSource answers a person's ACTIVE group ids. Implemented by
// component/memql over v1:identity:groupMembership, memoised per request on
// the same holder the account scope uses, so a request costs one membership
// read whichever gates it passes.
type MembershipSource interface {
	ActiveGroupIdsForUser(ctx context.Context, userId string) []string
}

var (
	installedGrantSource      atomic.Pointer[GrantSource]
	installedMembershipSource atomic.Pointer[MembershipSource]
)

// SetGrantSource installs (or, with nil, clears) the cluster's grant source.
// Called by the engine at start; clearing is for tests.
func SetGrantSource(s GrantSource) {
	if s == nil {
		installedGrantSource.Store(nil)
		return
	}
	installedGrantSource.Store(&s)
}

// InstalledGrantSource returns the installed source, or nil.
func InstalledGrantSource() GrantSource {
	if p := installedGrantSource.Load(); p != nil {
		return *p
	}
	return nil
}

// SetMembershipSource installs (or, with nil, clears) the membership source.
func SetMembershipSource(s MembershipSource) {
	if s == nil {
		installedMembershipSource.Store(nil)
		return
	}
	installedMembershipSource.Store(&s)
}

// InstalledMembershipSource returns the installed source, or nil.
func InstalledMembershipSource() MembershipSource {
	if p := installedMembershipSource.Load(); p != nil {
		return *p
	}
	return nil
}

// SubjectFromContext builds the subject for the VERIFIED caller on ctx: role
// and user id off the AccessContext, group ids from the installed membership
// source. Nothing a request could forge reaches it.
//
// The bool is "there is a caller at all". An anonymous or connector actor is a
// caller with no groups: neither is a person, so neither is a member of
// anything, and asking the source for `anonymous` would be a read for a row
// that cannot exist.
func SubjectFromContext(ctx context.Context) (Subject, bool) {
	ac, ok := AccessFromContext(ctx)
	if !ok {
		return Subject{}, false
	}
	s := Subject{
		// LOWERCASED, the principalOf discipline: AccessContext.Role is
		// stamped straight off the user row without folding case, and an
		// unfolded value ranks 0 and holds nothing.
		Role:     Role(normalizeSlug(string(ac.Role))),
		UserId:   strings.TrimSpace(ac.UserId),
		Unranked: ac.Unranked,
	}
	if s.Unranked || s.UserId == "" || ac.IsAnonymousActor() || ac.IsConnector() {
		return s, true
	}
	if ms := InstalledMembershipSource(); ms != nil {
		s.GroupIds = ms.ActiveGroupIdsForUser(ctx, s.UserId)
	}
	return s, true
}

// CapableFor is THE capability decision: may this subject do verb on
// resource? Every enforcement site on the request path -- the requires-
// capability gates, the data-plane gate, the groups guard -- resolves through
// this function, so there is exactly one definition of the rule in the header.
func CapableFor(ctx context.Context, subject Subject, verb, resource string) bool {
	held := roleHasCapability(Role(normalizeSlug(string(subject.Role))), verb, resource)
	if subject.Unranked {
		return held
	}
	src := InstalledGrantSource()
	if src == nil {
		return held
	}
	userGrants, groupGrants := grantsFor(ctx, src, subject)
	if v, decided := overlay(groupGrants, verb, resource); decided {
		held = v
	}
	if v, decided := overlay(userGrants, verb, resource); decided {
		held = v
	}
	return held
}

// overlay folds one level's grants over one pair: deny wins within the level,
// an allow decides otherwise, and a level with nothing to say leaves the
// previous answer standing. Inactive rows decide nothing.
func overlay(grants []Grant, verb, resource string) (held bool, decided bool) {
	sawAllow := false
	for _, g := range grants {
		if g.Inactive || g.Verb != verb || g.Resource != resource {
			continue
		}
		switch g.Effect {
		case GrantDeny:
			return false, true
		case GrantAllow:
			sawAllow = true
		}
	}
	if sawAllow {
		return true, true
	}
	return false, false
}

// grantsFor reads both levels for a subject, through the request memo when
// one is installed.
//
// A READ ERROR LEAVES THE CATALOG ANSWER STANDING. The alternative -- refuse
// everything when the rows cannot be read -- takes every person's access away
// because the database blinked, mid request, on one replica, which is the
// failure rbac_catalog.go's header argues against for roles. The request that
// blinked here fails on its own read a moment later anyway.
func grantsFor(ctx context.Context, src GrantSource, subject Subject) (user, group []Grant) {
	memo, _ := ctx.Value(grantMemoKey{}).(*grantMemo)
	if memo == nil {
		return readGrants(ctx, src, subject)
	}
	entry := memo.entryFor(subject)
	entry.once.Do(func() { entry.user, entry.group = readGrants(ctx, src, subject) })
	return entry.user, entry.group
}

func readGrants(ctx context.Context, src GrantSource, subject Subject) (user, group []Grant) {
	if u, err := src.ActiveGrantsForUser(ctx, subject.UserId); err == nil {
		user = u
	}
	if len(subject.GroupIds) > 0 {
		if g, err := src.ActiveGrantsForGroups(ctx, subject.GroupIds); err == nil {
			group = g
		}
	}
	return user, group
}

// grantMemo is the per-request holder, keyed BY SUBJECT rather than held as
// one value: a nested call under borrowed or system authority shares the
// request's context, and a memo that remembered the outer caller's grants
// would hand them to the inner one.
type grantMemo struct {
	mu      sync.Mutex
	entries map[string]*grantMemoEntry
}

type grantMemoEntry struct {
	once  sync.Once
	user  []Grant
	group []Grant
}

type grantMemoKey struct{}

func (m *grantMemo) entryFor(subject Subject) *grantMemoEntry {
	groups := append([]string(nil), subject.GroupIds...)
	sort.Strings(groups)
	key := subject.UserId + "\x1f" + strings.Join(groups, "\x1e")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]*grantMemoEntry{}
	}
	entry, ok := m.entries[key]
	if !ok {
		entry = &grantMemoEntry{}
		m.entries[key] = entry
	}
	return entry
}

// ContextWithGrantMemo installs the per-request memo, once. The engine calls
// it at its execute entry; a context that already carries one is returned
// unchanged, so nested entries share the request's memo.
func ContextWithGrantMemo(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(grantMemoKey{}).(*grantMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, grantMemoKey{}, &grantMemo{})
}

// GrantRowID derives the one id a (subjectKind, subjectId, verb, resourceType)
// tuple ever has -- the groupMembership discipline (D11): re-granting writes a
// new VERSION of one logical row rather than a second row, and revoking writes
// `active: false` at the same id.
//
// PER-PART DIGESTS, NOT A SEPARATOR (memql#3009). A resource type carries ':'
// and '/' (`app:deployables/publish`), and the id becomes the short id of a
// canonical `v1:rbac:grant:<id>` -- so a joined form would both collide on a
// moved boundary and put a colon where BareShortId splits. Each part is
// sha256-hex (a fixed 64 characters), so the concatenation has exactly one
// decomposition, and the whole is digested again so the id is one short token.
func GrantRowID(subjectKind, subjectId, verb, resourceType string) string {
	var joined strings.Builder
	for _, part := range []string{subjectKind, subjectId, verb, resourceType} {
		sum := sha256.Sum256([]byte(strings.TrimSpace(part)))
		joined.WriteString(hex.EncodeToString(sum[:]))
	}
	sum := sha256.Sum256([]byte(joined.String()))
	return hex.EncodeToString(sum[:])
}
