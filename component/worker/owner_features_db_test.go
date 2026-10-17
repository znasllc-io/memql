package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

func TestOwnerFeatureStoreReadsAnotherReplicasLatestConsent(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "owner feature settings", dbtest.DSN(), err)
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
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		return e
	}
	writer, reader := engine(), &EngineStore{Engine: engine()}
	owner := fmt.Sprintf("v1:identity:user:features-%d", time.Now().UnixNano())
	id := fmt.Sprintf("v1:worker:registration:features-%d", time.Now().UnixNano())
	payload, _ := json.Marshal(map[string]any{"ownerUserId": owner, "identityId": "credential", "name": "Feature machine", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]int{"HEADLESS": 1}, "registeredAt": time.Now().UTC().Format(time.RFC3339Nano)})
	row := memorynodes.MemoryNode{ID: id, CreatedAt: time.Now().UTC(), CreatedBy: owner, Concept: RegistrationConcept, Type: memorynodes.NodeTypeObject, Schema: json.RawMessage(`{}`), Payload: payload, Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`)}
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", RegistrationConcept).Where("id = ?", id).Exec(ctx)
	})
	before, err := reader.ReadOwnerFeaturePolicy(ctx, id, owner)
	if err != nil || before.Version.IsZero() || !before.Features.Headless {
		t.Fatalf("legacy policy: %+v %v", before, err)
	}
	call, err := langparser.RenderCall("setWorkerOwnerFeatures", map[string]any{"registrationId": id, "headless": false, "apps": true, "computerUse": false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Execute(auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)), call); err != nil {
		t.Fatal(err)
	}
	after, err := reader.ReadOwnerFeaturePolicy(ctx, id, owner)
	if err != nil || !after.Version.After(before.Version) || after.Features.Headless || !after.Features.Apps || after.Features.ComputerUse {
		t.Fatalf("new policy not read fresh across replicas: %+v %v", after, err)
	}
	if _, err := reader.ReadOwnerFeaturePolicy(ctx, id, "v1:identity:user:foreign"); err == nil {
		t.Fatal("foreign owner read a policy")
	}
}
