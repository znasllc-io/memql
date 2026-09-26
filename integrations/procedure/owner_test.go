package procedure

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type ownerLearningEngine struct {
	reads  int
	owner  string
	status string
}

func (e *ownerLearningEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.reads++
	if !strings.HasPrefix(query, "query workRunForOwner(") {
		return nil, fmt.Errorf("unexpected or privileged read: %s", query)
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.Synthetic || memql.BareShortId(ac.UserId) != "alice" {
		return memql.NewResultWithOutput([]map[string]any{}), nil
	}
	e.owner = ac.UserId
	return memql.NewResultWithOutput([]map[string]any{{"id": "run", "ownerUserId": "v1:identity:user:alice", "status": e.status}}), nil
}

func TestLearningTriggerBorrowsOnlyTheRecordedOwnerAcrossReplicas(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		owner     string
		wantError bool
		wantReads int
	}{
		{"owner", auth.ContextWithUserActor(context.Background(), "alice"), "bob", false, 1},
		{"other user", auth.ContextWithUserActor(context.Background(), "bob"), "alice", true, 1},
		{"missing actor", context.Background(), "alice", true, 0},
		{"untrusted source", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:learnFromSucceededRun", Synthetic: true}), "alice", true, 0},
		{"different system", auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "other")), "alice", true, 0},
		{"remote trusted event", auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:learnFromSucceededRun", Synthetic: true})), "alice", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ownerLearningEngine{status: "succeeded"}
			i := New(e, nil)
			_, err := i.handleLearnFromRun(tc.ctx, map[string]any{"runId": "run", "ownerUserId": tc.owner}, 0)
			if (err != nil) != tc.wantError || e.reads != tc.wantReads {
				t.Fatalf("err=%v reads=%d; want error=%v reads=%d", err, e.reads, tc.wantError, tc.wantReads)
			}
		})
	}
}

func TestLearningDoesNotMineAnUnfinishedRun(t *testing.T) {
	e := &ownerLearningEngine{status: "running"}
	i := New(e, nil)
	rows, err := i.handleLearnFromRun(auth.ContextWithUserActor(context.Background(), "alice"), map[string]any{"runId": "run"}, 0)
	if err != nil || len(rows) != 1 || !strings.Contains(fmt.Sprint(rows), "has not succeeded") || e.reads != 1 {
		t.Fatalf("unfinished run entered learning: rows=%v err=%v reads=%d", rows, err, e.reads)
	}
}
