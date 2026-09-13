package memql

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// TestDeveloperSiteSurvivesThePipelineAndGoesLive_DB is the production
// failure replayed through the REAL executor: the three writes a first deploy
// makes to a site row, in order, under the exact contexts the packages
// pipeline uses, followed by the click that was refused.
//
//  1. createSite, as the deploying developer -- EnsureSite's write.
//  2. recordSitePackageOrigin, under the developer's context WITH internal
//     origin stamped -- store.writeInternal's exact shape. Its delta names
//     only packageId and packageDeployableName.
//  3. updateSiteStatus(live), as the developer -- the OS's Go-live click.
//
// Before the fix, step 2 blanked ownerUserId (the privileged self-match undo
// fired on a stored value nothing had just stamped) and step 3 was refused:
// "row ... is not this caller's to write". The unit test beside this one pins
// the predicate; this one pins that the delta flag is captured where the
// executor can still see the delta, on the real template path.
func TestDeveloperSiteSurvivesThePipelineAndGoesLive_DB(t *testing.T) {
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "developer site survives pipeline (memql#4344 follow-up)", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	developer := "v1:identity:user:e2e-dev-" + suffix
	siteId := "v1:platform:site:e2e-" + suffix
	hostname := "e2e-" + suffix + "." + siteHostnamePolicyDomain()
	t.Cleanup(func() {
		_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", siteId).Exec(context.Background())
	})

	// The developer's request context: AccessContext for actor.* and the
	// row gate, TokenInfo for mutationActor. Both, as the gRPC layer sets.
	devCtx := auth.ContextWithToken(
		auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: developer, Role: auth.RoleDeveloper}),
		&auth.TokenInfo{Subject: developer},
	)
	// The pipeline's store.writeInternal: the SAME context, internal origin
	// stamped inline for one write.
	pipelineCtx := auth.ContextWithInternalOrigin(devCtx)

	owner := func() (owner, status string) {
		t.Helper()
		row := db.QueryRowContext(context.Background(),
			`select coalesce(payload->>'ownerUserId',''), coalesce(payload->>'status','') from "MemoryNodes" where id = ? order by "createdAt" desc limit 1`, siteId)
		require.NoError(t, row.Scan(&owner, &status))
		return owner, status
	}

	// 1. EnsureSite
	_, err = eng.Execute(devCtx, fmt.Sprintf(
		`mutation createSite(siteId: %s, hostname: %s, kind: "static", bundleRef: "", status: "draft")`,
		langparser.QuoteString(siteId), langparser.QuoteString(hostname)))
	require.NoError(t, err, "createSite as the developer")
	if got, _ := owner(); got != developer {
		t.Fatalf("after createSite ownerUserId = %q, want the developer %q", got, developer)
	}

	// 2. bindSiteToPackage -> writeInternal(recordSitePackageOrigin)
	_, err = eng.Execute(pipelineCtx, fmt.Sprintf(
		`mutation recordSitePackageOrigin(siteId: %s, packageId: %s, packageDeployableName: "storefront")`,
		langparser.QuoteString(siteId), langparser.QuoteString("v1:platform:package:e2e-"+suffix)))
	require.NoError(t, err, "recordSitePackageOrigin under internal origin")
	if got, _ := owner(); got != developer {
		t.Fatalf("after the pipeline's stamped update ownerUserId = %q, want %q still.\n"+
			"The privileged self-match undo blanked the developer's site into a cluster-owned row; "+
			"the Go-live click is now refused as \"not this caller's\"", got, developer)
	}

	// 3. Go live, as the developer -- the click that was refused in production.
	_, err = eng.Execute(devCtx, fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "live")`, langparser.QuoteString(siteId)))
	require.NoError(t, err, "updateSiteStatus(live) by the site's own developer")
	if got, status := owner(); got != developer || status != "live" {
		t.Fatalf("after Go live: ownerUserId=%q status=%q, want %q / live", got, status, developer)
	}
}
