package pipelinerun

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// Separate engines and connections share only Postgres. Cached reads on one
// replica must not override a permission change accepted by the other.
func TestChannelConfigurationAcrossRealReplicas(t *testing.T) {
	first := dbEngine(t)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	second, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := second.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	makeReplica := func(engine *memql.MemQLEngine) *Integration {
		lockDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
		t.Cleanup(func() { _ = lockDB.Close() })
		return New(Deps{Store: NewDSLStore(engine), Gate: func(ctx context.Context, key string, fn func(context.Context) error) error {
			return githubconnect.WithGate(ctx, lockDB, key, fn)
		}})
	}
	a, b := makeReplica(first), makeReplica(second)
	suffix := id.NewShortId()
	owner := "channel-owner-" + suffix
	ctx := signedIn(owner)
	p := Pipeline{ID: "channel-pipeline-" + suffix, OwnerUserID: owner, PackageID: "channel-package-" + suffix,
		Repository: "acme/channel-" + suffix, Name: "channels", DefaultBranch: "main", InstallationID: 7, CredentialID: "credential-" + suffix, Delivery: DeliveryWebhook}
	if err := a.snapshot().Store.CreatePipeline(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Prime the second replica's query cache before writes on the first.
	if _, err := b.snapshot().Store.PipelineForOwner(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			i := a
			if n%2 == 1 {
				i = b
			}
			name := fmt.Sprintf("releases-%d", n%2)
			if _, err := i.SaveChannel(ctx, ChannelSaveRequest{Name: name, Kind: "email", Recipients: []string{"owner@example.test"}}); err != nil {
				t.Error(err)
				return
			}
			if _, err := i.AllowChannel(ctx, p.ID, name, true); err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	channels, err := b.snapshot().Store.ChannelsForOwner(memql.ContextWithFreshRead(ctx))
	if err != nil || len(channels) != 2 {
		t.Fatalf("idempotent channels across replicas: %+v %v", channels, err)
	}
	current, err := b.snapshot().Store.PipelineForOwner(memql.ContextWithFreshRead(ctx), p.ID)
	if err != nil || current == nil || len(current.ChannelIDs) != 2 {
		t.Fatalf("concurrent grants lost: %+v %v", current, err)
	}
	driver := &runDriver{p: *current, d: b.snapshot()}
	step := pipelines.Step{Channel: "releases-0"}
	if _, failure := driver.targetOf(ctx, step); failure != nil {
		t.Fatalf("initial permission: %+v", failure)
	}
	if _, err := a.AllowChannel(ctx, p.ID, step.Channel, false); err != nil {
		t.Fatal(err)
	}
	if _, failure := driver.targetOf(ctx, step); failure == nil || failure.Code != pipelines.CodeChannelNotAllowed {
		t.Fatalf("cached grant survived revocation: %+v", failure)
	}
	if _, err := a.AllowChannel(ctx, p.ID, step.Channel, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ArchiveChannel(ctx, step.Channel); err != nil {
		t.Fatal(err)
	}
	if _, failure := driver.targetOf(ctx, step); failure == nil || failure.Code != pipelines.CodeChannelArchived {
		t.Fatalf("cached channel survived archive: %+v", failure)
	}
	stranger := signedIn("stranger-" + suffix)
	if rows, err := b.snapshot().Store.ChannelsForOwner(stranger); err != nil || len(rows) != 0 {
		t.Fatalf("foreign channel disclosure: %+v %v", rows, err)
	}
	if _, err := b.AllowChannel(stranger, p.ID, step.Channel, false); err == nil {
		t.Fatal("foreign permission change accepted")
	}
}
