//go:build clustere2e

package clustere2e

// Cross-node actor propagation over a mesh forward, LIVE (memql#3205).
//
// CONTRACT under test. An actor-gated construct executed on the WORKER side of
// a BFF -> worker forward must bind the SAME actor the BFF resolved. Before the
// forwarded-auth contract it bound none: the receiver attached the sender's raw
// claims and never built an AccessContext, so under the deny-on-nil default
// (memql#2801) actor.userId resolved to "" and every owned filter
// (ownerUserId == actor.userId) matched nothing. The failure was SILENT -- zero
// rows, no error -- which is why it survived in production on the live path
// (memql#2876) after the same defect was fixed on the client-facing stream
// (memql#216).
//
// WHY THIS HAS TO BE A CLUSTER TEST. A single-node run never crosses the hop:
// shouldProxyAI returns false without a worker peer, the tool executes
// in-process on the BFF's own session, and the actor is present for the boring
// reason that it never left. The defect is only reachable when the handler runs
// on a session built from an AiForwardRequest -- which needs a real second
// node. Per CLAUDE.md, a green single-node test is a false signal for this
// whole bug class.
//
// SHAPE. `notesList` is a query-backed tool (@handler query="query notes()")
// that targets the agent node, and `notes` is an OWNED query -- the row set is
// gated by ownerUserId == actor.userId server-side. So:
//
//	1. seed a uniquely-marked note on the DIRECT path (ownership is
//	   server-stamped from the BFF's resolved AccessContext);
//	2. read it back on the DIRECT path -- proves the seed landed and pins the
//	   expectation, so a failure in step 3 cannot be blamed on the fixture;
//	3. read it back through CallTool, which the BFF FORWARDS to an agent node.
//
// Step 3 is the assertion. Same actor on the far side -> the marked note comes
// back. No actor (the defect) -> the owned filter yields zero rows and the
// marker is missing. A mis-resolved actor (the reverted attempt's unclamped
// role, or a different principal) -> also missing, because the note is owned by
// the seeding user and nobody else.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestForwardedActor_CrossNode
//
// or `make cluster-e2e`. Requires the 2-replica parity cluster
// (`make up SERVERS=2` + `make scale N=2`) so a worker peer exists for the BFF
// to forward to -- see docs/public/operate/reproduce-staging-locally.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

func TestForwardedActor_CrossNode(t *testing.T) {
	tok := token(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 2)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	// A marker unique to this run, so a stale note from an earlier run can
	// never make the assertion pass for the wrong reason.
	marker := "memql3205-" + id.NewShortId()
	noteId := "v1:notes:note:" + id.NewShortId()

	// 1. Seed on the direct path. ownerUserId is server-stamped from the
	//    AccessContext the BFF resolved for this stream -- the client never
	//    supplies it, which is the point: there is no way to forge ownership.
	if _, err := qc.CreateNote(ctx, memqlclient.CreateNoteArgs{
		NoteId: noteId,
		Title:  marker,
		Body:   "seeded by the memql#3205 forwarded-actor gate",
		Tags:   []string{"memql3205"},
	}); err != nil {
		t.Fatalf("seed note on the direct path: %v", err)
	}

	// 2. Direct read-back. This pins the expectation: if the marker is missing
	//    HERE the fixture is broken (seed failed, replication lag, wrong user),
	//    and step 3 would be untrustworthy either way.
	if !eventuallyContainsMarker(ctx, t, marker, func(ctx context.Context) (string, error) {
		res, err := qc.Notes(ctx, memqlclient.NotesArgs{})
		if err != nil {
			return "", err
		}
		return renderResult(res)
	}) {
		t.Fatalf("seeded note %q is not visible on the DIRECT path; the fixture is broken, not the forward", marker)
	}

	// 3. THE ASSERTION. CallTool -> the BFF forwards to an agent node ->
	//    the agent runs `query notes()` on a session it built from the
	//    AiForwardRequest. The owned filter resolves against whatever actor
	//    that session bound.
	forwarded, err := qc.CallTool(ctx, memqlclient.CallToolArgs{
		Name:      "notesList",
		Arguments: map[string]any{"limit": 200},
	})
	if err != nil {
		t.Fatalf("CallTool(notesList) across the forward: %v", err)
	}
	if forwarded.IsError {
		t.Fatalf("CallTool(notesList) returned an error result: %s", renderToolContent(forwarded))
	}

	body := renderToolContent(forwarded)
	if !strings.Contains(body, marker) {
		t.Fatalf(`the receiving node did not bind the caller's actor.

The note %q is owned by this caller and IS visible on the direct path, but the
forwarded execution of the same owned query did not return it. That is the
memql#3205 signature: with no AccessContext on the worker side, actor.userId
resolves to "" and ownerUserId == actor.userId matches nothing -- silently.

forwarded result: %s`, marker, truncate(body, 2000))
	}
}

// eventuallyContainsMarker polls a read until the marker appears or the budget
// runs out. The seed is a graph write and the read is a separate round trip
// that may land on a different replica, so a short settle window keeps this
// gate about actor binding rather than about write visibility timing.
func eventuallyContainsMarker(
	ctx context.Context,
	t *testing.T,
	marker string,
	read func(context.Context) (string, error),
) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := read(ctx)
		if err != nil {
			lastErr = err
		} else if strings.Contains(out, marker) {
			return true
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		t.Logf("last read error while waiting for %q: %v", marker, lastErr)
	}
	return false
}

// renderResult flattens a query result to a string for marker matching. The
// gate cares whether the caller's own row crossed, not about its shape.
func renderResult(res *memqlclient.Result) (string, error) {
	if res == nil {
		return "", fmt.Errorf("nil result")
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return string(b), nil
}

// renderToolContent concatenates a tool result's content blocks.
func renderToolContent(res *memqlclient.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		b.WriteString(c.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}
