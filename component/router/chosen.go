package router

// chosen.go -- the two facts about a call that decide which ACCOUNT may pay for
// it at a vendor (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K8). Neither routes anything.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// chosenByActor reports whether the acting person chose how this call is
// served: they pinned the model themselves, or they chose the route it walks.
//
// K8 lines 4 and 5 turn on it. A person who picks API in Ask, or a route of
// their own, or pins a vendor model, has said their key may pay. Auto, and
// every call nobody chose a route for, counts only keys whose owner switched
// Auto on. So a choice somebody ELSE made is not a choice: an agent's stored
// pin is its owner's, and a run shared with another person keeps its opener's
// route.
//
// A PIN OUTRANKS A ROUTE, as it does in resolution: with a model pinned, who
// pinned it is the whole answer.
func chosenByActor(req ResolveRequest) bool {
	actor := strings.TrimSpace(req.UserId)
	if actor == "" {
		return false
	}
	if strings.TrimSpace(req.ExplicitProvider) != "" {
		return samePerson(req.PinnedBy, actor)
	}
	if strings.TrimSpace(req.PersonRoute) != "" || strings.TrimSpace(req.Route) != "" {
		return samePerson(req.RouteBy, actor)
	}
	return false
}

// samePerson compares two user ids that may differ only in being canonical or
// bare. Two blanks are nobody, never the same person.
func samePerson(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || memql.BareShortId(a) == memql.BareShortId(b)
}

// visitorCall reports whether this call serves a visitor under a person's
// borrowed authority, so that no API key pays for it (K8 line 2):
//
//   - the request's own context was borrowed for a visitor: a shopper form or
//     an access-code call (auth.ContextWithVisitorBorrow);
//   - the automation running declared the deployer's fleet, so it acts for the
//     person who deployed the workflow rather than for whoever triggered it;
//   - the run it belongs to was opened by such a request (the request arrives
//     here already marked, from the run row: memql.ApplyRunRouting).
//
// Each of those looks, to everything else, like the owner acting. This is the
// one place they are told apart.
func visitorCall(ctx context.Context, req ResolveRequest) bool {
	if req.ForVisitor || auth.BorrowedForVisitor(ctx) {
		return true
	}
	_, declared := memql.DeployerFleetDeclaration(ctx)
	return declared
}
