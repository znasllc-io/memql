//go:build clustere2e

package clustere2e

// forwarded_actor_test.go -- memql#3205, the cross-node gate.
//
// CLAUDE.md: "a green single-node test is a FALSE signal for cross-node
// behaviour". Everything else in this change is in-process; this is the case
// that runs the hop for real.
//
// It CANNOT RUN WITHOUT A LIVE 2-REPLICA CLUSTER, and it does not run in CI:
// the whole package is behind //go:build clustere2e, `token(t)` skips without
// MEMQL_E2E_TOKEN, and the endpoint defaults to the k3d front door. Run it with
//
//	make up SERVERS=2 && make scale N=2 && make cluster-e2e
//
// The property: `notesCreate` / `notesList` are actor-gated -- their DSL
// filters on `ownerUserId == actor.userId`, and the tool surface carries no
// ownerUserId arg, so ownership is server-stamped from the auth context. That
// makes them the sharpest available probe for "did the receiving node bind the
// caller's actor?":
//
//   - if the forward binds NO actor (the memql#2876 defect), the create stamps
//     `createdBy: ""` and the list returns ZERO rows under the deny-on-nil
//     default -- silently, which is what made the defect survive so long;
//   - if the forward binds the WRONG actor, the note is invisible to the user
//     who created it.
//
// Both failures look like "the note vanished", which is exactly the assertion
// below.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// A tool call proxied BFF -> agent must execute under the CALLER's actor on the
// receiving node.
func TestForwardedToolCallBindsTheCallersActorOnTheWorker(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	noteId := "v1:notes:note:" + id.NewShortId()
	marker := "forwarded-actor-probe-" + id.NewShortId()

	// CallTool is one of the payloads AiForwardRouter proxies to the agent
	// node, so this create executes on the far side of the hop.
	created, err := qc.CallTool(ctx, memqlclient.CallToolArgs{
		Name: "notesCreate",
		Arguments: map[string]any{
			"noteId": noteId,
			"title":  "forwarded actor probe",
			"body":   marker,
		},
	})
	if err != nil {
		t.Fatalf("notesCreate through the forwarded CallTool surface: %v", err)
	}
	if created.IsError {
		t.Fatalf("notesCreate reported an error: %+v", created.Content)
	}

	// And read it back as the same user. `notes` filters on
	// ownerUserId == actor.userId, so the row is only visible if the WRITE on
	// the receiving node stamped this caller as the owner.
	listed, err := qc.CallTool(ctx, memqlclient.CallToolArgs{
		Name:      "notesList",
		Arguments: map[string]any{"limit": 100},
	})
	if err != nil {
		t.Fatalf("notesList through the forwarded CallTool surface: %v", err)
	}
	if listed.IsError {
		t.Fatalf("notesList reported an error: %+v", listed.Content)
	}

	var body strings.Builder
	for _, c := range listed.Content {
		body.WriteString(c.Text)
	}
	if !strings.Contains(body.String(), marker) {
		t.Errorf("the note created through a forwarded tool call is not visible to the user who "+
			"created it (marker %q absent from %d content fragment(s)).\n\n"+
			"`notesCreate` stamps ownerUserId from the auth context and `notes` filters on "+
			"ownerUserId == actor.userId, so this fails in exactly two ways, both of them the "+
			"reason memql#3205 exists: the receiving node bound NO actor (createdBy stamped empty, "+
			"deny-on-nil returns zero rows) or it bound the WRONG one. Neither raises an error "+
			"anywhere -- the row simply is not there.",
			marker, len(listed.Content))
	}
}
