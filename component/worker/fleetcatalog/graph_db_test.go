package fleetcatalog_test

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
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/worker/fleetcatalog"
)

// A recording store proves actor plumbing, but cannot prove the DSL actually
// excludes another owner's private machine. Use real persisted registrations
// and the same reader the BFF and agent install, with no worker stream at all.
func TestFleetCatalogGraphReaderEnforcesOwnerAndSharedBoundaries(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "fleet catalog graph read", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	engine.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("catalog-read-%d", time.Now().UnixNano())
	alice := "v1:identity:user:" + prefix + "-alice"
	bob := "v1:identity:user:" + prefix + "-bob"
	var ids []string
	t.Cleanup(func() {
		if len(ids) > 0 {
			_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", "v1:worker:registration").Where("id IN (?)", bun.In(ids)).Exec(ctx)
		}
	})
	carol := "v1:identity:user:" + prefix + "-carol"
	for _, spec := range []struct {
		name, owner, sharing, serve string
		userIds                     []string
	}{
		{"mine", alice, "private", "owner", nil},
		{"private", bob, "private", "owner", nil},
		{"shared", bob, "cluster", "cluster", nil},
		{"owner-consent-only", bob, "cluster", "owner", nil},
		// Epic memql#5344: lent to NAMED people. Alice's catalog includes the
		// one lent to her and never the one lent to carol (design G8).
		{"lent-alice", bob, "people", "cluster", []string{alice}},
		{"lent-carol", bob, "people", "cluster", []string{carol}},
	} {
		id := "v1:worker:registration:" + prefix + "-" + spec.name
		ids = append(ids, id)
		sharing := map[string]any{"mode": spec.sharing}
		if spec.userIds != nil {
			sharing["userIds"] = spec.userIds
		}
		raw, _ := json.Marshal(map[string]any{"name": spec.name, "ownerUserId": spec.owner, "capabilities": []string{"MODEL"}, "labels": map[string]string{"model:" + prefix + "-" + spec.name: "ctx=32768,structured=1,params=27300000000"}, "sharing": sharing, "capabilityDescriptor": map[string]string{"inferenceServe": spec.serve}, "lastSeenAt": time.Now().UTC().Format(time.RFC3339Nano), "connectedNodeId": "another-agent-replica"})
		row := memorynodes.MemoryNode{ID: id, CreatedAt: time.Now().UTC(), CreatedBy: spec.owner, Concept: "v1:worker:registration", Type: memorynodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: raw, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
		if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		reader := &fleetcatalog.Reader{Store: &fleetcatalog.EngineStore{Engine: engine}}
		engine.Providers().SetFleetCatalog(reader)
		owner, err := reader.Catalog(ctx, alice)
		if err != nil {
			t.Fatal(err)
		}
		// A PERSON'S catalog is their own machines plus the ones lent to them
		// (design G8) -- it used to be their own only, which this test pinned,
		// and which shut a person out of every machine a colleague lent them.
		mineSeen := map[string]bool{}
		for _, model := range owner {
			mineSeen[model.ModelId] = true
			if model.ModelId == prefix+"-mine" && !model.Online() {
				t.Fatalf("alice's own machine should read online: %+v", model)
			}
		}
		for _, want := range []string{"mine", "shared", "lent-alice"} {
			if !mineSeen[prefix+"-"+want] {
				t.Fatalf("alice's catalog %v is missing %s", mineSeen, want)
			}
		}
		for _, leak := range []string{"private", "owner-consent-only", "lent-carol"} {
			if mineSeen[prefix+"-"+leak] {
				t.Fatalf("alice's catalog leaked %s: %v", leak, mineSeen)
			}
		}
		shared, err := reader.Catalog(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, model := range shared {
			seen[model.ModelId] = true
		}
		if !seen[prefix+"-shared"] || seen[prefix+"-private"] || seen[prefix+"-owner-consent-only"] || seen[prefix+"-mine"] ||
			seen[prefix+"-lent-alice"] || seen[prefix+"-lent-carol"] {
			t.Fatalf("shared catalog = %v; system work reaches only what is lent to everyone (design G3)", seen)
		}
		// Run the actual public builtin on a node with a catalog but no dispatcher.
		result, err := engine.Execute(auth.ContextWithUserActor(ctx, alice), "builtin fleetModels()")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(result.OutputPayload())
		if err != nil {
			t.Fatal(err)
		}
		var nodes map[string]memorynodes.MemoryNode
		if err := json.Unmarshal(raw, &nodes); err != nil {
			t.Fatal(err)
		}
		if _, ok := nodes[prefix+"-mine"]; !ok {
			t.Fatalf("public builtin lost owner model: %s", raw)
		}
		if _, ok := nodes[prefix+"-shared"]; !ok {
			t.Fatalf("public builtin lost shared model: %s", raw)
		}
		if _, ok := nodes[prefix+"-lent-alice"]; !ok {
			t.Fatalf("public builtin lost the model lent to alice: %s", raw)
		}
		for _, name := range []string{"private", "owner-consent-only", "lent-carol"} {
			if _, ok := nodes[prefix+"-"+name]; ok {
				t.Fatalf("public builtin leaked %s", name)
			}
		}
		if engine.Providers().FleetInferenceInstalled() {
			t.Fatal("graph reads installed a dispatcher")
		}
	}
}
