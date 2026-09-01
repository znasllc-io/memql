package campaigns

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// server_only_origin_test.go -- the gate this package cannot fail quietly
// (memql#4820).
//
// Nine of the domain's mutations carry @serverOnly. The executor refuses one
// unless auth.OriginFromContext(ctx).IsInternal(), and the refusal is not a
// degradation: the call fails EVERY TIME, on EVERY cluster, and the only trace
// on the way past is a WARN. A drain worker whose recordCampaignDelivery is
// refused writes no ledger row, which means the resume diff finds nobody dealt
// with, which means it mails the same audience again on the next tick. The
// worst outcome in the domain, reached by an omitted context argument.
//
// So this test drives every @serverOnly store method against an engine that
// records the ORIGIN of the context it was called with, and asserts internal.
// The converse half matters just as much and is asserted too: an ordinary
// write must NOT be stamped, or `execServerOnly` would be a blanket rather
// than a split, and the file would stop telling a reader which writes this
// package claims are engine-initiated.
//
// # Why this exists alongside the repo-root gate
//
// TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin matches
// `mutation <name>(` inside STRING LITERALS, at FILE granularity. Both limits
// bite here:
//
//   - SetCampaignStatus builds its call from a mutationName VARIABLE, so
//     startCampaign / pauseCampaign / resumeCampaign appear in no literal and
//     are invisible to it;
//   - store.go stamps SOMEWHERE, so the file-level check is satisfied by any
//     one of the calls even if another were routed through plain `exec`.
//
// This is the per-operation pin the root gate names as its own limitation.

// originEngine records, per call, whether the context carried internal origin.
// Deliberately its own fixture rather than the package's fakeEngine: a
// package-level fake shared across files is how one test's fixture rows leak
// into another's assertions.
type originEngine struct {
	mu    sync.Mutex
	calls []originCall
}

type originCall struct {
	query    string
	internal bool
}

func (e *originEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, originCall{query: q, internal: auth.OriginFromContext(ctx).IsInternal()})
	return map[string]any{"nodes": []any{}}, nil
}

func (e *originEngine) last() originCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.calls) == 0 {
		return originCall{}
	}
	return e.calls[len(e.calls)-1]
}

// serverOnlyWrites is every @serverOnly mutation this package issues, paired
// with the store method that issues it. Keeping the CONSTRUCT NAME next to the
// call is what makes a drift visible: rename a mutation in the DSL and this
// list stops matching the rendered call, which fails loudly rather than
// silently checking a call nobody makes.
func serverOnlyWrites() []struct {
	construct string
	issue     func(*Store, context.Context) error
} {
	return []struct {
		construct string
		issue     func(*Store, context.Context) error
	}{
		{"recordCampaignDelivery", func(s *Store, ctx context.Context) error {
			return s.RecordDelivery(ctx, Delivery{
				CampaignID: "c1", RecipientID: "r1", Email: "a@example.test",
				Status: "sent", SentAt: time.Now().UTC(), Attempts: 1,
			})
		}},
		{"updateCampaignProgress", func(s *Store, ctx context.Context) error {
			sent := 3
			return s.UpdateCampaignProgress(ctx, "c1", CampaignProgress{SentCount: &sent})
		}},
		{"scheduleCampaign", func(s *Store, ctx context.Context) error {
			return s.ScheduleCampaign(ctx, "c1", time.Now().UTC())
		}},
		{"startCampaign", func(s *Store, ctx context.Context) error {
			return s.SetCampaignStatus(ctx, "startCampaign", "c1")
		}},
		{"pauseCampaign", func(s *Store, ctx context.Context) error {
			return s.SetCampaignStatus(ctx, "pauseCampaign", "c1")
		}},
		{"resumeCampaign", func(s *Store, ctx context.Context) error {
			return s.SetCampaignStatus(ctx, "resumeCampaign", "c1")
		}},
		{"recordEngagementEvent", func(s *Store, ctx context.Context) error {
			return s.RecordEngagementEvent(ctx, EngagementEvent{
				CampaignID: "c1", DeliveryID: "d1", Kind: "open", OccurredAt: time.Now().UTC(),
			})
		}},
	}
}

func TestServerOnlyWritesStampInternalOrigin(t *testing.T) {
	for _, w := range serverOnlyWrites() {
		engine := &originEngine{}
		store := NewStore(engine)
		if err := w.issue(store, context.Background()); err != nil {
			t.Fatalf("%s: %v", w.construct, err)
		}
		call := engine.last()
		if !strings.Contains(call.query, "mutation "+w.construct+"(") {
			t.Fatalf("%s: the rendered call is %q, which does not name the construct this case claims to cover",
				w.construct, call.query)
		}
		if !call.internal {
			t.Errorf("%s ran WITHOUT internal origin.\n"+
				"  rendered: %s\n"+
				"The mutation is @serverOnly, so the executor refuses it outright -- this call cannot "+
				"succeed on any cluster. Route it through Store.execServerOnly, which stamps inline at "+
				"the one Execute that needs it.", w.construct, call.query)
		}
	}
}

// TestOrdinaryWritesAreNotStamped is the converse, and it is the half that
// keeps execServerOnly a SPLIT rather than a blanket.
//
// recordSuppression and updateSendJob are clusterOwner-tier and NOT
// @serverOnly: reaching them already requires the engine's own operator
// identity, which is an authorization fact rather than a channel one. Stamping
// them would mark a context that has no business being marked, and -- worse --
// would remove the one signal in store.go that says which writes this package
// claims are engine-initiated.
func TestOrdinaryWritesAreNotStamped(t *testing.T) {
	cases := []struct {
		name  string
		issue func(*Store, context.Context) error
	}{
		{"recordSuppression", func(s *Store, ctx context.Context) error {
			return s.RecordSuppression(ctx, strings.Repeat("a", 64), "manual", "example.test", "", "")
		}},
		{"updateSendJob", func(s *Store, ctx context.Context) error {
			status := "queued"
			return s.UpdateJob(ctx, "job-1", SendJobPatch{Status: &status})
		}},
		{"enqueueCampaignSend", func(s *Store, ctx context.Context) error {
			return s.EnqueueSend(ctx, SendJob{
				CampaignID: "c1", CampaignOwnerUserID: "u1", AudienceID: "a1", TemplateID: "t1",
			})
		}},
		{"setRecipientSubscription", func(s *Store, ctx context.Context) error {
			return s.SetRecipientSubscription(ctx, "r1", "unsubscribed", time.Now().UTC())
		}},
	}
	for _, c := range cases {
		engine := &originEngine{}
		store := NewStore(engine)
		if err := c.issue(store, context.Background()); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if engine.last().internal {
			t.Errorf("%s was stamped with internal origin. It is not @serverOnly, so the stamp buys "+
				"nothing and costs the signal: store.go's two exec paths are how a reader tells which "+
				"writes are engine-initiated.", c.name)
		}
	}
}
