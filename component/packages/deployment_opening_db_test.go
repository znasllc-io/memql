package packages

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func TestDeploymentOpeningExcludesConcurrentReplicas(t *testing.T) {
	first, db := dbEngine(t)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	second, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = discardLogger()
	if err := second.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	for _, sameID := range []bool{true, false} {
		t.Run(fmt.Sprintf("sameID=%v", sameID), func(t *testing.T) {
			suffix := fmt.Sprintf("opening-%d", time.Now().UnixNano())
			owner := "v1:identity:user:" + suffix
			packageID := "v1:platform:package:" + suffix
			ids := []string{"v1:platform:packageDeployment:" + suffix + "-retry-2", "v1:platform:packageDeployment:" + suffix + "-other"}
			seedTiePrincipal(t, first, owner, auth.RoleOwner)
			ctx := tieActorCtx(owner, auth.RoleOwner)
			account := "account-" + suffix
			mustExecute(t, first, ctx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Opening test")`, langparser.QuoteString(account)))
			mustExecute(t, first, ctx, fmt.Sprintf(`mutation createPackage(packageId: %s, name: "Opening test", sourceKind: "repo", accountId: %s, repoUrl: %s)`, langparser.QuoteString(packageID), langparser.QuoteString(account), langparser.QuoteString("https://github.com/test/"+suffix)))
			t.Cleanup(func() {
				_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id LIKE ?", "%"+suffix+"%").Exec(context.Background())
			})
			stores := []*store{{engine: first, directDB: func() *sql.DB { return db.DB }}, {engine: second, directDB: func() *sql.DB { return db.DB }}}
			start := make(chan struct{})
			errs := make(chan error, 2)
			for n, s := range stores {
				go func(n int, s *store) {
					<-start
					id := ids[n]
					if sameID {
						id = ids[0]
					}
					// Wire callers can use a bare package ID; it must share a gate.
					pkg := packageID
					if n == 1 {
						pkg = memql.BareShortId(pkg)
					}
					errs <- s.openDeployment(ctx, deploymentSeed{DeploymentId: id, PackageId: pkg, OwnerUserId: owner, AccountId: account, RequestedBy: owner, Automatic: true, SourceVersion: "same-commit", StartedAt: time.Now()})
				}(n, s)
			}
			close(start)
			succeeded, refused := 0, 0
			for n := 0; n < 2; n++ {
				err := <-errs
				if err == nil {
					succeeded++
				} else if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "already has a live deployment") {
					refused++
				} else {
					t.Fatal(err)
				}
			}
			if succeeded != 1 || refused != 1 {
				t.Fatalf("opening race: successes=%d refusals=%d", succeeded, refused)
			}
			live, err := stores[0].liveDeploymentsForPackage(ctx, packageID)
			if err != nil || len(live) != 1 {
				t.Fatalf("expected one live attempt: %v %v", live, err)
			}
			winner := rowString(live[0], "id")
			if err := stores[0].closeDeployment(ctx, deploymentClose{DeploymentId: winner, Status: StatusCancelled, FinishedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := stores[1].openDeployment(ctx, deploymentSeed{DeploymentId: winner, PackageId: packageID, OwnerUserId: owner, AccountId: account, StartedAt: time.Now()}); err == nil {
				t.Fatal("canceled attempt was reopened")
			}
			fresh := ids[0]
			if sameShortId(fresh, winner) {
				fresh = ids[1]
			}
			if err := stores[1].openDeployment(ctx, deploymentSeed{DeploymentId: fresh, PackageId: packageID, OwnerUserId: owner, AccountId: account, StartedAt: time.Now()}); err != nil {
				t.Fatalf("new attempt after cancellation should open: %v", err)
			}
		})
	}
}

func TestDeploymentOpeningWithoutDirectDatabaseFailsClosed(t *testing.T) {
	e := &recordingEngine{}
	s := &store{engine: e}
	if err := s.openDeployment(context.Background(), deploymentSeed{PackageId: "package", DeploymentId: "attempt"}); err == nil {
		t.Fatal("missing direct DB admitted a deployment")
	}
	if len(e.statements()) != 0 {
		t.Fatal("missing gate reached the graph")
	}
}
