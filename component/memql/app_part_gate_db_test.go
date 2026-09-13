package memql

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE DEPLOYABLES PARTS, END TO END AGAINST A REAL ENGINE (epic memql#5288,
// task memql#5301, third criterion).
//
// The unit tests beside this prove the gate over a bare engine. This proves
// the SHIPPED DECLARATIONS reach it through the real call paths: the builtin
// `packageDeploy` parsed from dsl/platform/builtins.memql and dispatched
// through the top-level builtin branch, and the mutation `updateSiteStatus`
// parsed from dsl/platform/mutations.memql and dispatched through the
// mutation entry point. A `user`-role caller holding `read app:deployables`
// and `execute app:deployables/deploy` -- and nothing on the publish part --
// reaches the deploy executor and is refused the publish mutation before
// any row is read.
//
// THE GRANT IS INSTALLED ON THE CATALOG, NOT WRITTEN AS A ROW. The shared
// engine never runs the seed materializer, so the seeded rows are not there
// to lean on, and the per-person grant concept is the engine epic's
// (memql#5287); what this test needs is a catalog that answers "this
// caller's role holds the deploy part and not the publish part", which the
// fake every other requires-capability test in this package uses states
// exactly. The negative control -- the same caller with NO grant on the
// deploy part is refused packageDeploy -- is what proves the executor was
// reached because of the grant rather than because nothing was asked.
//
// Postgres-gated like its neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

const partGateDeployExecutor = "integration.packages.deploy"

// installPartGateCatalog: `writer` (the member tier's user-row spelling) holds
// the deploy part when granted is true and never the publish part;
// `developer` holds both, as the seeds say.
func installPartGateCatalog(t *testing.T, granted bool) {
	t.Helper()
	open := auth.VerbResource{Verb: auth.VerbRead, Resource: "app:deployables"}
	deploy := auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"}
	publish := auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/publish"}
	writer := map[auth.VerbResource]bool{}
	if granted {
		writer[open] = true
		writer[deploy] = true
	}
	auth.SetCapabilityCatalog(&capabilityFake{
		ranks: map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "viewer": 50},
		grants: map[string]map[auth.VerbResource]bool{
			"owner":     {open: true, deploy: true, publish: true},
			"developer": {open: true, deploy: true, publish: true},
			"admin":     {},
			"user":      writer,
			"writer":    writer,
			"viewer":    {},
		},
	})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

// installDeployRecorder stands a recording executor behind the shipped
// packageDeploy builtin for one test. Set on the engine's dispatch map
// directly rather than through RegisterIntegration, which refuses a second
// registration under the same name and has no unregister -- the shared
// engine outlives this test.
func installDeployRecorder(t *testing.T, eng *MemQLEngine) *int {
	t.Helper()
	if eng.builtinExecutorHandlers == nil {
		if err := eng.initBuiltinExecutorHandlers(); err != nil {
			t.Fatalf("init builtin handlers: %v", err)
		}
	}
	reached := 0
	previous, had := eng.builtinExecutorHandlers[partGateDeployExecutor]
	eng.builtinExecutorHandlers[partGateDeployExecutor] = func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		reached++
		return nil, nil
	}
	t.Cleanup(func() {
		if had {
			eng.builtinExecutorHandlers[partGateDeployExecutor] = previous
		} else {
			delete(eng.builtinExecutorHandlers, partGateDeployExecutor)
		}
	})
	return &reached
}

func partGateActor(userId string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

func isCapabilityRefusal(err error, part string) bool {
	return err != nil && strings.Contains(err.Error(), "requires the execute on app:deployables/"+part+" capability")
}

func TestAUserGrantedTheDeployPartRunsPackageDeployAndIsRefusedGoLive(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	member := "v1:identity:user:part-member-" + suffix
	packageId := "v1:platform:package:part-pkg-" + suffix
	siteId := "v1:platform:site:part-site-" + suffix

	deployCall := fmt.Sprintf(`builtin packageDeploy(packageId: %s, confirm: true)`, langparser.QuoteString(packageId))
	goLive := fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "live")`, langparser.QuoteString(siteId))

	// ---- granted: the deploy part opens packageDeploy ...
	installPartGateCatalog(t, true)
	reached := installDeployRecorder(t, eng)
	if _, err := eng.Execute(partGateActor(member, auth.RoleWriter), deployCall); err != nil {
		t.Fatalf("a user-role caller granted execute on app:deployables/deploy was refused packageDeploy: %v", err)
	}
	if *reached != 1 {
		t.Fatalf("packageDeploy did not reach its executor for the granted caller (reached=%d)", *reached)
	}

	// ... and NOT updateSiteStatus(live), which is the publish part. Refused
	// before the row is read: there is no site row, and the error must be
	// the capability refusal rather than "not found".
	_, err := eng.Execute(partGateActor(member, auth.RoleWriter), goLive)
	if !isCapabilityRefusal(err, "publish") {
		t.Fatalf("a user-role caller holding only the deploy part was not refused the publish part on updateSiteStatus(live): %v", err)
	}

	// ---- a developer holds the publish part, so the gate lets the mutation
	// through to the row layer -- where THIS site does not exist. Whatever
	// that answers, it is not the capability refusal.
	_, err = eng.Execute(partGateActor("v1:identity:user:part-dev-"+suffix, auth.RoleDeveloper), goLive)
	if isCapabilityRefusal(err, "publish") {
		t.Fatalf("a developer, who the seeds grant every part, was refused the publish part: %v", err)
	}
}

// TestAUserWithoutTheDeployPartIsRefusedPackageDeploy is the negative control
// for the test above: the same caller, the same call, no grant -- refused
// before the executor runs.
func TestAUserWithoutTheDeployPartIsRefusedPackageDeploy(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	member := "v1:identity:user:part-nobody-" + suffix
	packageId := "v1:platform:package:part-pkg-" + suffix

	installPartGateCatalog(t, false)
	reached := installDeployRecorder(t, eng)
	_, err := eng.Execute(partGateActor(member, auth.RoleWriter),
		fmt.Sprintf(`builtin packageDeploy(packageId: %s, confirm: true)`, langparser.QuoteString(packageId)))
	if !isCapabilityRefusal(err, "deploy") {
		t.Fatalf("a user-role caller with no grant on the deploy part was not refused packageDeploy by the capability gate: %v", err)
	}
	if *reached != 0 {
		t.Fatal("packageDeploy reached its executor for a caller the gate refused")
	}
}
