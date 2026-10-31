package memql

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

// site_access_events_db_test.go -- a write to an access code or a session publishes no
// graph event (design D14; re-review of Task 3).
//
// ===========================================================================
// WHAT WENT WRONG
// ===========================================================================
// The read clause closes every way a CLIENT can read these rows, and the event bus is
// not a client. executeWrite publishes graph.node.created (and executeUpdate
// graph.node.updated) with the whole stored row flattened into the envelope and retained
// again under "payload", and events.Bus.Publish matches on topic and fans a clone out to
// every subscriber: component/events has no AccessContext and no authorization hook of any
// kind. An authored automation can trigger on a concept (and an email rule is an authored
// automation), and an automation's run journal writes the event it ran for into a run row
// that its author, and a cluster owner, can read. The re-review did exactly that: an
// authored automation on node.created of the code concept fired on a mint, and the run row
// held the digest and the label.
//
// The ruling is the staged-data tier's: withholdGraphWriteEvent answers true for the
// concepts on clientUnreadableConcepts, and it guards the BUILD of the envelope as well as
// the publish, at both emit sites. Cache invalidation is outside it by design (it carries
// {"concept": <id>} and nothing else), so the result cache still hears about the write.

// busRecorder collects every event a bus carries, as the JSON a subscriber would see.
type busRecorder struct {
	mu     sync.Mutex
	topics []string
	bodies []string
}

func (r *busRecorder) record(ev events.Event) {
	raw, _ := json.Marshal(ev)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.topics = append(r.topics, ev.Topic)
	r.bodies = append(r.bodies, string(raw))
}

func (r *busRecorder) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.topics...), append([]string(nil), r.bodies...)
}

func (r *busRecorder) seen(topic string) bool {
	topics, _ := r.snapshot()
	for _, got := range topics {
		if got == topic {
			return true
		}
	}
	return false
}

func TestNoGraphEventIsPublishedForAnAccessCodeOrSession(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	previous := eng.EventBus()
	bus := events.NewBus()
	eng.SetEventBus(bus)
	t.Cleanup(func() { eng.SetEventBus(previous) })
	recorder := &busRecorder{}
	t.Cleanup(bus.Subscribe("#", recorder.record))

	ids := newSiteAccessIDs("events")
	issued := time.Now().UTC().Truncate(time.Second)
	stamp := func(d time.Duration) string { return issued.Add(d).Format(time.RFC3339) }
	owner := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, siteAccessAs(ids.owner, auth.RoleWriter)))
	label := "label-for-the-bus-" + ids.code
	digest, regenerated, token := siteAccessDigest(0x81), siteAccessDigest(0x83), siteAccessDigest(0x82)

	// Every kind of write the two concepts take: a mint, a regenerate, a re-time, a removal,
	// a session opened and a session ended. The updates travel the executeUpdate emit as
	// well as executeWrite's.
	for _, call := range []string{
		siteAccessCall(t, "mutation", "createSiteAccessCode", map[string]any{
			"codeId": ids.code, "siteId": ids.site, "accountId": ids.account, "label": label, "gate": "dashboard",
			"codeHash": digest, "codeHint": "FBZA", "issuedAt": stamp(0), "expiresAt": stamp(time.Hour), "lastChangedBy": ids.owner,
		}),
		siteAccessCall(t, "mutation", "rotateSiteAccessCode", map[string]any{
			"codeId": ids.code, "codeHash": regenerated, "codeHint": "ZZZZ", "generation": 2,
			"issuedAt": stamp(time.Minute), "expiresAt": stamp(2 * time.Hour), "lastChangedBy": ids.owner,
		}),
		siteAccessCall(t, "mutation", "setSiteAccessCodeExpiry", map[string]any{
			"codeId": ids.code, "expiresAt": stamp(3 * time.Hour), "lastChangedBy": ids.owner,
		}),
		siteAccessCall(t, "mutation", "createSiteAccessSession", map[string]any{
			"sessionId": ids.session, "siteId": ids.site, "codeId": ids.code, "generation": 2,
			"tokenHash": token, "issuedAt": stamp(2 * time.Minute),
		}),
		siteAccessCall(t, "mutation", "endSiteAccessSession", map[string]any{
			"sessionId": ids.session, "endedAt": stamp(3 * time.Minute),
		}),
		siteAccessCall(t, "mutation", "removeSiteAccessCode", map[string]any{
			"codeId": ids.code, "revokedAt": stamp(4 * time.Minute), "lastChangedBy": ids.owner,
		}),
	} {
		_, err := eng.Execute(owner, call)
		require.NoError(t, err, call)
	}

	// THE CONTROL: a write to an ordinary concept does publish, so a silent bus here would
	// mean a harness that cannot hear anything. It is written LAST, and its arrival bounds
	// how long the earlier events (delivered in goroutines) could still be in flight.
	seedGroup(t, eng, "g-"+ids.account, "Events", "account", ids.account)
	require.Eventually(t, func() bool { return recorder.seen("graph.node.created.v1:identity:group") },
		5*time.Second, 20*time.Millisecond, "the control write published no graph event, so this test cannot hear the bus")
	time.Sleep(300 * time.Millisecond)

	topics, bodies := recorder.snapshot()
	for _, topic := range topics {
		require.False(t, strings.HasPrefix(topic, "graph.") && (strings.Contains(topic, "siteAccessCode") || strings.Contains(topic, "siteAccessSession")),
			"a write to an access code or session published %s", topic)
	}
	// Nothing of either row rides any other topic either.
	for i, body := range bodies {
		for _, secret := range []string{digest, regenerated, token, label} {
			require.NotContains(t, body, secret, "the event on %s carries a value of an access code or session", topics[i])
		}
	}

	// INVALIDATION STILL WORKS: the result cache and the other replicas hear of the write on
	// their own channel, which carries the concept's name and nothing of its rows.
	for _, concept := range []string{siteAccessCodeConcept, siteAccessSessionConcept} {
		require.True(t, recorder.seen(events.TopicCacheInvalidateForConcept(concept)),
			"no cache invalidation was published for %s, so the result cache would keep a stale answer", concept)
	}
}
