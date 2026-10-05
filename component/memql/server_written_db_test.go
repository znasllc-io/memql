package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Exercise the actual raw-write seam, not the named @serverOnly mutations.
// The accompanying pipelinerun database suite proves the legitimate writer
// paths (connect, trigger, poll, recovery, cancel and notify) remain admitted.
func TestServerWrittenPipelineRecordsRefuseRawClientWrites(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	for _, name := range []string{"pipeline", "run", "channel"} {
		kind := "v1:pipelines:" + name
		meta, err := eng.concepts.Get(kind)
		if err != nil || !meta.ServerWritten {
			t.Fatalf("%s must declare @serverWritten: %+v %v", kind, meta, err)
		}
		for _, role := range []string{"writer", "owner", "system"} {
			ctx := actorCtx(map[string]any{"sub": "v1:identity:user:record-owner", "role": role})
			for _, verb := range []string{"insert", "update"} {
				t.Run(name+"/"+role+"/"+verb, func(t *testing.T) {
					id := fmt.Sprintf("%s:forged-%d", kind, time.Now().UnixNano())
					// Raw insert is a public expression. Update reaches the same
					// seam as a lowered mutation, not a public update() function.
					err := attemptServerRecordWrite(eng, ctx, verb, kind, id, `{"ownerUserId":"v1:identity:user:record-owner","status":"completed","conclusion":"success"}`)
					if err == nil || !strings.Contains(err.Error(), "@serverWritten requires internal origin") {
						t.Fatalf("raw %s must be refused by the concept contract: %v", verb, err)
					}
					n, err := db.NewSelect().Model((*concept.MemoryNode)(nil)).Where("concept = ? AND id = ?", kind, id).Count(context.Background())
					if err != nil || n != 0 {
						t.Fatalf("refused write stored %d rows: %v", n, err)
					}
				})
			}
		}
	}
}

func TestServerWrittenChannelAllowsInternalWriteButNotClientRewrite(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	const kind = "v1:pipelines:channel"
	id := fmt.Sprintf("guard-%d", time.Now().UnixNano())
	ctx := auth.ContextWithUserActor(context.Background(), "v1:identity:user:record-owner")
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`mutation createPipelineChannel(channelId: %s, name: "guard-test", kind: "email", recipients: ["test@example.test"])`, langparser.QuoteString(id))); err != nil {
		t.Fatalf("validated server write: %v", err)
	}
	canonical := kind + ":" + id
	for _, verb := range []string{"insert", "update"} {
		err := attemptServerRecordWrite(eng, ctx, verb, kind, canonical, `{"name":"forged","status":"archived"}`)
		if err == nil || !strings.Contains(err.Error(), "@serverWritten") {
			t.Fatalf("client %s rewrote a real server row: %v", verb, err)
		}
	}
	if got := latestPayload(t, context.Background(), db, kind, canonical)["name"]; got != "guard-test" {
		t.Fatalf("refused rewrite changed the row: %v", got)
	}
}

func attemptServerRecordWrite(eng *MemQLEngine, ctx context.Context, verb, kind, id, payload string) error {
	if verb == "insert" {
		_, err := eng.Execute(ctx, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(kind), langparser.QuoteString(id), payload))
		return err
	}
	_, _, err := eng.executeWrite(ctx, MutationNode{Concept: kind, ID: id, PayloadRaw: payload}, true)
	return err
}
