package memql

// THE ENGINE'S GRANT SOURCE (app access grants record, section 1; epic
// memql#5296).
//
// component/auth declares what a grant resolver IS (grant_resolver.go); this
// file is the one that reads the rows. It is the grant-shaped twin of
// rbac_catalog.go with one deliberate difference: NOTHING IS CACHED. The role
// catalog is loaded once and reloaded on its events because every request
// reads it; grants are read PER REQUEST, memoised on the context for that
// request alone, and forgotten (D9). So there is no snapshot to go stale, no
// cross-node reload to wire, and a grant written on one replica is honoured on
// every other at the next request because every request reads the rows.
//
// # Under the engine's own identity
//
// The rows are read with bun directly, as the catalog's are, and not through
// the admin-floored DSL reads: a user-role actor has to be served THEIR OWN
// deny, and a read floored at admin could not do that. This is an
// authorization read (staged-data: MUST-NOT-GATE, for the membership reads'
// reason -- hiding a row here removes a decision somebody made, and a hidden
// deny fails OPEN).

import (
	"context"
	"strings"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// conceptRbacGrant is the canonical concept id of the grant rows.
const conceptRbacGrant = "v1:rbac:grant"

// graphGrantSource implements auth.GrantSource over v1:rbac:grant rows.
type graphGrantSource struct{ engine *MemQLEngine }

func (s graphGrantSource) ActiveGrantsForUser(ctx context.Context, userId string) ([]auth.Grant, error) {
	return s.engine.grantsForSubjects(ctx, auth.SubjectKindUser, conceptIdentityUser, []string{userId})
}

func (s graphGrantSource) ActiveGrantsForGroups(ctx context.Context, groupIds []string) ([]auth.Grant, error) {
	return s.engine.grantsForSubjects(ctx, auth.SubjectKindGroup, conceptIdentityGroup, groupIds)
}

// graphMembershipSource implements auth.MembershipSource over the memoised
// membership resolution the account scope already performs, so the data-plane
// gate and the groups guard -- which build their subject through
// auth.SubjectFromContext -- read the same memo the engine's own gates do.
type graphMembershipSource struct{ engine *MemQLEngine }

func (s graphMembershipSource) ActiveGroupIdsForUser(ctx context.Context, userId string) []string {
	return s.engine.membershipsFor(ctx, userId).groupIds
}

// InstallGrantResolution installs this engine as the cluster's grant and
// membership source. Called from StartCapabilityCatalog at engine start, and
// explicitly by the database-gated tests, whose engines never Start.
func (e *MemQLEngine) InstallGrantResolution() {
	if e == nil {
		return
	}
	auth.SetGrantSource(graphGrantSource{engine: e})
	auth.SetMembershipSource(graphMembershipSource{engine: e})
}

// subjectFor builds the capability subject for the caller on ctx: role and id
// off the AccessContext, groups from the memoised membership resolution.
//
// Engine-side rather than through auth.SubjectFromContext so the
// requires-capability gates answer from THIS engine's memo whether or not a
// membership source has been installed -- an engine that has not Started, or
// a test engine, still resolves its own memberships.
func (e *MemQLEngine) subjectFor(ctx context.Context) (auth.Subject, bool) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok {
		return auth.Subject{}, false
	}
	s := auth.Subject{
		Role:     auth.Role(strings.ToLower(strings.TrimSpace(string(ac.Role)))),
		UserId:   strings.TrimSpace(ac.UserId),
		Unranked: ac.Unranked,
	}
	if s.Unranked || s.UserId == "" || ac.IsAnonymousActor() || ac.IsConnector() {
		return s, true
	}
	s.GroupIds = e.membershipsFor(ctx, s.UserId).groupIds
	return s, true
}

// grantsForSubjects reads the newest version of every grant naming any of the
// subjects, in either spelling of their ids.
//
// The WHERE is on id-derived fields only (subjectKind and subjectId are part
// of the derived row id and cannot change across versions), so the
// `DISTINCT ON (id)` collapse still picks the newest version of each row.
// `active` is NOT in the WHERE for exactly that reason: an older active
// version must not be selected over a newer inactive one. It rides out on the
// Grant as Inactive and the resolver decides what it means.
func (e *MemQLEngine) grantsForSubjects(ctx context.Context, kind, subjectConcept string, ids []string) ([]auth.Grant, error) {
	db := e.database()
	if db == nil {
		return nil, errNoDatabaseForCatalog
	}
	spellings := make([]string, 0, len(ids)*3)
	seen := map[string]struct{}{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		spellings = append(spellings, v)
	}
	for _, id := range ids {
		add(id)
		if bare := BareShortId(id); bare != "" {
			add(bare)
			add(subjectConcept + ":" + bare)
		}
	}
	if len(spellings) == 0 {
		return nil, nil
	}
	var nodes []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&nodes).
		DistinctOn("id").
		Where("concept = ?", conceptRbacGrant).
		Where("payload->>'subjectKind' = ?", kind).
		Where("payload->>'subjectId' IN (?)", bun.In(spellings)).
		OrderExpr(`id ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]auth.Grant, 0, len(nodes))
	for i := range nodes {
		payload := rankRowPayload(nodes[i])
		if payload == nil {
			continue
		}
		g := auth.Grant{
			Verb:     strings.TrimSpace(stringFromAny(payload["verb"])),
			Resource: strings.TrimSpace(stringFromAny(payload["resourceType"])),
			// EFFECT DEFAULTS ALLOW, matching the catalog reader: only the
			// literal "deny" narrows.
			Effect: auth.GrantAllow,
		}
		if strings.TrimSpace(stringFromAny(payload["effect"])) == auth.GrantDeny {
			g.Effect = auth.GrantDeny
		}
		if v, present := payload["active"].(bool); present && !v {
			g.Inactive = true
		}
		if g.Verb == "" || g.Resource == "" {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}
