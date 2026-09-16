package sitehealth

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

type visibleEngine struct {
	sites []Site
	actor string
}

func (e *visibleEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	a, _ := auth.AccessFromContext(ctx)
	if a != nil {
		e.actor = a.UserId
	}
	rows := []any{}
	for _, s := range e.sites {
		rows = append(rows, map[string]any{"id": s.ID, "hostname": s.Hostname, "bundleRef": s.BundleRef, "status": s.Status})
	}
	return memql.NewResultWithOutput(rows), nil
}

type measuredChecker struct{}

func (measuredChecker) Check(_ context.Context, s Site) Observation {
	return Observation{SiteID: s.ID, Hostname: s.Hostname, BundleRef: s.BundleRef, CheckedAt: time.Now().UTC(), State: "unavailable", HTTPStatus: 404, Reason: "Website returned HTTP 404"}
}

// A connection-local TEMP table shadows production storage. The actual migration
// and UPSERT run against Postgres, with no writes to any live observations.
func TestObservationStorageAndAuthorizedRead(t *testing.T) {
	dsn := os.Getenv("MEMQL_SITE_HEALTH_TEST_DSN")
	if dsn == "" {
		dsn = dbtest.DSN()
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "website health", dsn, err)
		return
	}
	migration, err := os.ReadFile("../database/memory-nodes/migrations/20260915230000_site_health.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, strings.Replace(string(migration), "CREATE TABLE IF NOT EXISTS", "CREATE TEMP TABLE", 1)); err != nil {
		t.Fatal(err)
	}
	e := &visibleEngine{sites: []Site{{"one", "one.example.com", "blob://one", "live"}}}
	i := &Integration{engine: e, db: func() *bun.DB { return db }, checker: measuredChecker{}}
	maintenance := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "system:maintenance:checkDeployableHealth", Role: auth.RoleOwner})
	for range 2 {
		if _, err := i.sweep(maintenance, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.NewRaw("SELECT count(*) FROM site_health").Scan(ctx, &count); err != nil || count != 1 {
		t.Fatalf("one latest observation: count=%d err=%v", count, err)
	}
	caller := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "reader", Role: auth.RoleReader})
	rows, err := i.read(caller, map[string]any{"siteIds": []string{"v1:platform:site:one", "hidden"}}, 0)
	if err != nil || len(rows) != 1 || e.actor != "reader" {
		t.Fatalf("current caller's query must gate the read: rows=%d actor=%s err=%v", len(rows), e.actor, err)
	}
	var o Observation
	if err := json.Unmarshal(rows[0].Payload, &o); err != nil {
		t.Fatal(err)
	}
	if o.State != "unavailable" || o.HTTPStatus != 404 {
		t.Fatalf("failed check lost: %+v", o)
	}
	e.sites[0].BundleRef = "blob://new"
	rows, err = i.read(caller, map[string]any{"siteIds": []string{"one"}}, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("prior deployment observation leaked: %v %v", rows, err)
	}
	e.sites = nil
	rows, err = i.read(caller, map[string]any{"siteIds": []string{"one"}}, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("invisible site observation leaked: %v %v", rows, err)
	}
	if _, err = i.sweep(maintenance, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.NewRaw("SELECT count(*) FROM site_health").Scan(ctx, &count); err != nil || count != 0 {
		t.Fatalf("retired observations retained: %d %v", count, err)
	}
}
