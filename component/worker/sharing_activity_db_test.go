package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestSharingActivityPersistsAcrossReplicasAndRejectsFormerStreams(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "sharing activity", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine := func() *memql.MemQLEngine {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return e
	}
	first, second := engine(), engine()
	owner := fmt.Sprintf("v1:identity:user:activity-%d", time.Now().UnixNano())
	machine := fmt.Sprintf("v1:worker:registration:activity-%d", time.Now().UnixNano())
	payload, _ := json.Marshal(map[string]any{"ownerUserId": owner, "identityId": "credential", "name": "Activity machine", "capabilities": []string{"MODEL"}, "concurrency": map[string]int{"MODEL": 2}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "connectedNodeId": "agent-a"})
	row := memorynodes.MemoryNode{ID: machine, CreatedAt: time.Now().UTC(), CreatedBy: owner, Concept: "v1:worker:registration", Type: memorynodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: payload, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", "v1:worker:registration").Where("id = ?", machine).Exec(ctx)
	})
	a, b := &EngineStore{Engine: first}, &EngineStore{Engine: second}
	start := time.Now().UTC()
	report := func(at time.Time, n int) map[string]any {
		return map[string]any{"nodeId": "agent-a", "startedAt": at.Format(time.RFC3339Nano), "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "users": []map[string]any{{"id": "ana", "activeCalls": n}}, "groups": []any{}}
	}
	if err := a.UpdateSharingActivity(ctx, machine, owner, "agent-a", report(start, 2)); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		nodes, err := b.executeAndExtract(auth.ContextWithUserActor(memql.ContextWithFreshRead(ctx), owner), fmt.Sprintf("query workersForUser(ownerUserId:%s)", langparser.QuoteString(owner)))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if SameSubjectId(n.GetId(), machine) {
				return newWorkerFieldGetter(n).anyMap("sharingActivity")
			}
		}
		t.Fatal("missing machine")
		return nil
	}
	if fmt.Sprint(read()["users"]) != "[map[activeCalls:2 id:ana]]" {
		t.Fatalf("the sibling did not see counts: %+v", read())
	}
	newer := start.Add(time.Second)
	if err := b.UpdateSharingActivity(ctx, machine, owner, "agent-a", report(newer, 0)); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateSharingActivity(ctx, machine, owner, "agent-a", report(start, 9)); err != nil {
		t.Fatal(err)
	}
	if read()["startedAt"] != newer.Format(time.RFC3339Nano) {
		t.Fatal("a former stream replaced the successor")
	}
	if err := a.UpdateSharingActivity(ctx, machine, owner, "another-agent", report(newer.Add(time.Second), 8)); err != nil {
		t.Fatal(err)
	}
	if read()["startedAt"] != newer.Format(time.RFC3339Nano) {
		t.Fatal("a former replica replaced the current holder")
	}
	if err := a.UpdateSharingActivity(ctx, machine, "v1:identity:user:someone-else", "agent-a", report(newer.Add(time.Second), 8)); err == nil {
		t.Fatal("an unowned registration was writable")
	}
}
