package app

// groups_placement.go -- placing an arriving person in their groups (epic
// memql#5165, section G).
//
// The two ARRIVAL SEAMS -- an invitation accepted, a first sign-in -- both end
// with a user row and an address. This is what turns that into memberships:
// the groups the invitation named, and then the account whose verified domain
// matches a verified address.
//
// # It lives in app/ because it is WIRING
//
// component/identity must not depend on integrations/, and integrations/groups
// must not know what an invitation is. Both halves are right, so the node --
// which already holds every store it is assembling -- is where they meet. The
// seams take a function; this builds it.
//
// # It never fails an arrival
//
// Every path logs and returns. By the time it runs the user row exists and the
// invitation is spent, so there is no failure it could report that the caller
// could act on: an error would render "your invitation could not be accepted"
// to somebody whose account was just created, and leave them unable to retry
// because the invitation is single-use. A missing membership is one click for
// an admin; a lost arrival is a support ticket.

import (
	"context"
	"log/slog"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/groups"
)

// placerEngine narrows the engine to the one method integrations/groups needs.
//
// It exists because the two Execute signatures differ: the engine returns a
// typed *ExecuteResult and the groups package -- which must not import
// component/memql's result type to stay a narrow seam -- takes `any`. The
// plug-in factory has the same adapter for the same reason; this is the one
// caller that builds the integration directly rather than through the
// registry, and it needs its own.
//
// GO DOES NOT COERCE THE SIGNATURE, so leaving this out compiles everywhere
// except under the `identity` build tag, which is the only build that reaches
// the wiring below. That failure is invisible to `go build ./...` and to
// `make test`.
type placerEngine struct{ engine *memql.MemQLEngine }

func (p placerEngine) Execute(ctx context.Context, query string) (any, error) {
	return p.engine.Execute(ctx, query)
}

// groupPlacer places arrivals. Nil-safe throughout: a node with no groups
// integration wired places nobody and says nothing.
type groupPlacer struct {
	groups *groups.Integration
	logger *slog.Logger
}

// placeInvitedUser writes one membership per group the invitation carried,
// then applies domain join.
//
// The address is treated as VERIFIED: the link was delivered to it and the
// person holding it just followed it, which is the same evidence a magic-link
// sign-in rests on (design D9).
//
// THE GROUPS ARE NOT RE-JUDGED. Each was validated at ISSUE against an active
// group and against the rank rule, and the inviter -- whose authority the rank
// rule is about -- is gone by now. Re-checking here would check the invitee's
// rank instead, which is a different question with a different answer.
func (p *groupPlacer) placeInvitedUser(ctx context.Context, userId, email string, groupIds []string, issuedBy string) {
	if p == nil || p.groups == nil {
		return
	}
	// Internal origin plus a cluster-owner actor: the writes below are the
	// deployment placing a row on the strength of a decision an admin
	// already made, and the arriving person has no authority of their own
	// yet -- they are being provisioned.
	writeCtx := auth.ContextWithInternalOrigin(groups.SystemActorContext(ctx))
	for _, groupId := range groupIds {
		if err := p.groups.PlaceInvitedMember(writeCtx, groupId, userId, issuedBy); err != nil {
			p.warn("could not place an invited person in a group",
				"user", userId, "group", groupId, "error", err.Error())
		}
	}
	p.applyDomainJoin(ctx, userId, email, true)
}

// invitationJoinsOrganization answers D10 again when an invitation whose role
// is scoped to `accountId` is accepted: is one of the groups it carries still
// an active group of that organization? It runs BEFORE the account is
// written, so a "no" leaves nothing behind.
//
// UNLIKE PLACEMENT, THIS IS A JUDGEMENT, and that is why it may fail the
// arrival where placement may not: placing somebody in a dead group is a
// membership an admin adds in one click, while a role held outside its
// organization is the state D10 exists to prevent. An unreadable group is a
// "no" for the same reason.
func (p *groupPlacer) invitationJoinsOrganization(ctx context.Context, groupIds []string, accountId string) bool {
	if p == nil || p.groups == nil {
		return false
	}
	readCtx := auth.ContextWithInternalOrigin(groups.SystemActorContext(ctx))
	for _, groupId := range groupIds {
		joins, err := p.groups.GroupJoinsOrganization(readCtx, groupId, accountId)
		if err != nil {
			p.warn("could not read an invitation's group, so its scoped role was refused",
				"group", groupId, "organization", accountId, "error", err.Error())
			return false
		}
		if joins {
			return true
		}
	}
	return false
}

// placeFirstSignIn applies domain join to somebody arriving without an
// invitation.
//
// `emailVerified` is passed by the seam that KNOWS -- the OIDC path has the
// provider's claim, the magic-link path proved the address by delivering to
// it, and the bootstrap owner has proven nothing yet. There is deliberately no
// default: a guessed one would either join people on unverified claims or
// silently join nobody.
func (p *groupPlacer) placeFirstSignIn(ctx context.Context, userId, email string, emailVerified bool) {
	if p == nil || p.groups == nil {
		return
	}
	p.applyDomainJoin(ctx, userId, email, emailVerified)
}

func (p *groupPlacer) applyDomainJoin(ctx context.Context, userId, email string, emailVerified bool) {
	writeCtx := auth.ContextWithInternalOrigin(groups.SystemActorContext(ctx))
	groupId, err := p.groups.ApplyDomainJoin(writeCtx, userId, email, emailVerified)
	if err != nil {
		p.warn("domain join failed", "user", userId, "error", err.Error())
		return
	}
	if groupId != "" && p.logger != nil {
		p.logger.Info("domain join placed an arriving person in a client's group",
			"user", userId, "group", groupId)
	}
}

func (p *groupPlacer) warn(msg string, args ...any) {
	if p == nil || p.logger == nil {
		return
	}
	p.logger.Warn("groups: "+msg, args...)
}
