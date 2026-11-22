package router

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// A route the acting person chose, or a model they pinned themselves, may be
// paid by their key. Anything else is Auto, where a key pays only when its
// owner switched that on (design record 2026-11-21, K8 lines 4 and 5).
func TestChosenByActor(t *testing.T) {
	for name, tc := range map[string]struct {
		req  ResolveRequest
		want bool
	}{
		"a model the person pinned":           {ResolveRequest{UserId: "alice", ExplicitProvider: "claudeSonnet", PinnedBy: "alice"}, true},
		"the same person, canonical and bare": {ResolveRequest{UserId: "v1:identity:user:alice", ExplicitProvider: "claudeSonnet", PinnedBy: "alice"}, true},
		"a model somebody else pinned":        {ResolveRequest{UserId: "alice", ExplicitProvider: "claudeSonnet", PinnedBy: "bob"}, false},
		"a pin that is nobody's":              {ResolveRequest{UserId: "alice", ExplicitProvider: "claudeSonnet"}, false},
		"a route the person chose":            {ResolveRequest{UserId: "alice", PersonRoute: "api", RouteBy: "alice"}, true},
		"a policy route the person chose":     {ResolveRequest{UserId: "alice", Route: "localFirst", RouteBy: "alice"}, true},
		"a route somebody else chose":         {ResolveRequest{UserId: "alice", PersonRoute: "api", RouteBy: "bob"}, false},
		"a route with nobody named":           {ResolveRequest{UserId: "alice", PersonRoute: "api"}, false},
		"auto":                                {ResolveRequest{UserId: "alice"}, false},
		"auto, with a stale RouteBy":          {ResolveRequest{UserId: "alice", RouteBy: "alice"}, false},
		"nobody acting":                       {ResolveRequest{PersonRoute: "api", RouteBy: ""}, false},
		"a pin outranks the route beside it":  {ResolveRequest{UserId: "alice", ExplicitProvider: "claudeSonnet", PinnedBy: "bob", PersonRoute: "api", RouteBy: "alice"}, false},
		// An app session's subject is the person, and what it pins or chooses
		// is attributed to them. It is still not them choosing.
		"a pin an app session made as the person":    {ResolveRequest{UserId: "alice", ExplicitProvider: "claudeSonnet", PinnedBy: "alice", ChoiceByMachine: true}, false},
		"a route an app session chose as the person": {ResolveRequest{UserId: "alice", PersonRoute: "api", RouteBy: "alice", ChoiceByMachine: true}, false},
	} {
		if got := chosenByActor(tc.req); got != tc.want {
			t.Errorf("%s: chosenByActor = %v, want %v", name, got, tc.want)
		}
	}
}

// A call is a visitor's when the request's context borrowed a person's
// authority for one (a shopper form, an access-code call), when the automation
// running declared the deployer's fleet, or when the run it belongs to was
// opened by such a request. Nothing a caller sets makes or clears it.
func TestAVisitorsCallIsMarked(t *testing.T) {
	owner := auth.ContextWithUserActor(context.Background(), "u-merchant")
	for name, tc := range map[string]struct {
		ctx  context.Context
		req  ResolveRequest
		want bool
	}{
		"the owner's own call":             {owner, ResolveRequest{UserId: "u-merchant"}, false},
		"a shopper form or an access code": {auth.ContextWithVisitorBorrow(owner), ResolveRequest{UserId: "u-merchant"}, true},
		"a deployed workflow's deployer":   {memql.ContextWithDeployerFleet(owner, true, "shop"), ResolveRequest{UserId: "u-merchant"}, true},
		"an automation that declared none": {memql.ContextWithDeployerFleet(owner, false, ""), ResolveRequest{UserId: "u-merchant"}, false},
		"a run a visitor's request opened": {owner, ResolveRequest{UserId: "u-merchant", ForVisitor: true}, true},
	} {
		if got := visitorCall(tc.ctx, tc.req); got != tc.want {
			t.Errorf("%s: visitorCall = %v, want %v", name, got, tc.want)
		}
	}
}
