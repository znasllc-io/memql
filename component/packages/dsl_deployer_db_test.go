package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// dsl_deployer_db_test.go -- a @usesDeployerFleet workflow finding its person, against
// a real engine (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md).
//
// The seam's own tests answer its two reads with a stub. What they cannot prove is that
// those reads WORK as the cluster: that the package's row tier admits the actor, that a
// @serverOnly query opens under the stamp, and that the domain filter matches a list.

func TestADeclaredWorkflowFindsItsDeployerThroughTheRealEngine(t *testing.T) {
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	domain := "ddfleet" + strings.ReplaceAll(suffix, "-", "")

	developer := "v1:identity:user:ddf-dev-" + suffix
	admin := "v1:identity:user:ddf-adm-" + suffix
	pkg := "v1:platform:package:ddf-pkg-" + suffix
	second := "v1:platform:package:ddf-two-" + suffix
	t.Cleanup(func() {
		for _, id := range []string{pkg, second, developer, admin} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	seedTiePrincipal(t, eng, developer, auth.RoleDeveloper)
	seedTiePrincipal(t, eng, admin, auth.RoleAdmin)
	ownerCtx := tieActorCtx(developer, auth.RoleDeveloper)
	createTiedPackage(t, eng, ownerCtx, pkg, "ddf-"+suffix, "ddf-acct-"+suffix)
	s := &store{engine: eng, logger: discardLogger(), directDB: func() *sql.DB { return db.DB }}

	declared := func(d string) context.Context {
		return memql.ContextWithDeployerFleet(auth.ContextWithSystemActor(context.Background(), "ddf-workflow"), true, d)
	}
	refusal := func(t *testing.T, d, want string) {
		t.Helper()
		_, err := eng.ContextForDeployerFleet(declared(d))
		var refused *memql.DeployerFleetRefusal
		if !errors.As(err, &refused) || !strings.Contains(refused.Reason, want) {
			t.Fatalf("err = %v, want a refusal saying %q", err, want)
		}
	}

	refusal(t, domain, "no active package ships this domain")

	if err := s.recordPackageDslDomains(ownerCtx, pkg, []string{"other" + domain, domain}); err != nil {
		t.Fatal(err)
	}
	refusal(t, domain, "no person is recorded")

	if err := s.recordPackageDslDeployer(ownerCtx, pkg, developer); err != nil {
		t.Fatal(err)
	}
	row, err := s.packageById(ownerCtx, pkg)
	if err != nil || row == nil || rowString(row, "dslDeployedBy") != developer {
		t.Fatalf("the record does not come back through the package's shape: %v (%v)", row, err)
	}

	bound, err := eng.ContextForDeployerFleet(declared(domain))
	if err != nil {
		t.Fatalf("a package with one deployer refused: %v", err)
	}
	access, ok := auth.AccessFromContext(bound)
	if !ok || access.UserId != developer {
		t.Fatalf("the call acts for %+v, want %s", access, developer)
	}
	if _, ok := auth.ForwardedAuthorityFromContext(bound); !ok {
		t.Fatal("the call carries no authority for the hop")
	}

	// D7: the record is read again on every call, and the role with it.
	if err := s.recordPackageDslDeployer(ownerCtx, pkg, admin); err != nil {
		t.Fatal(err)
	}
	refusal(t, domain, "may no longer author constructs")
	if err := s.recordPackageDslDeployer(ownerCtx, pkg, developer); err != nil {
		t.Fatal(err)
	}

	// D6: a second package listing the domain leaves no answer.
	createTiedPackage(t, eng, ownerCtx, second, "ddf-two-"+suffix, "ddf-acct-"+suffix)
	if err := s.recordPackageDslDomains(ownerCtx, second, []string{domain}); err != nil {
		t.Fatal(err)
	}
	refusal(t, domain, "2 active packages ship this domain")
}
