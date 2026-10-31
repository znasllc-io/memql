package packages

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/edge"
)

// dsl_deployer_test.go -- WHO a package's @usesDeployerFleet workflows act for
// (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md, D3 and D4).

func deployerWrites(h *harness) []string {
	var out []string
	for _, q := range h.engine.statements() {
		if !strings.HasPrefix(q, "mutation recordPackageDslDeployer(") {
			continue
		}
		_, after, _ := strings.Cut(q, "dslDeployedBy: ")
		out = append(out, strings.TrimSuffix(after, ")"))
	}
	return out
}

func packageDeployedBy(person string) map[string]any {
	row := ownerPackage()
	row["dslDeployedBy"] = person
	return row
}

func actorNamed(id string) Actor {
	return Actor{UserId: id, MayDeployDsl: true}
}

func TestADeployThatStagesDslRecordsItsActor(t *testing.T) {
	h := newHarness(t, validPackage(), packageDeployedBy("bob"))
	if _, err := deployConfirmed(h, actorNamed("alice")); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if h.roller.rolls != 1 {
		t.Fatalf("setup: the DSL was meant to stage and roll (rolls %d)", h.roller.rolls)
	}
	if writes := deployerWrites(h); len(writes) != 1 || writes[0] != `"alice"` {
		t.Fatalf("the run recorded %v, want its actor", writes)
	}
	stmts := h.engine.statements()
	if statementIndex(stmts, "mutation recordPackageDslDeployer(") < statementIndex(stmts, "mutation recordPackageDslDomains(") {
		t.Fatal("the person was recorded before the commit point the domain list keeps")
	}
}

func TestADeployThatDoesNotMoveTheDslKeepsThePerson(t *testing.T) {
	tree := validPackage()
	h := newHarness(t, tree, packageDeployedBy("bob"))
	h.stager.active = activeSetMatching(t, h, tree, "acme")
	if _, err := deployConfirmed(h, actorNamed("alice")); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if h.roller.rolls != 0 {
		t.Fatalf("setup: the DSL was meant to be unchanged (rolls %d)", h.roller.rolls)
	}
	if writes := deployerWrites(h); len(writes) != 0 {
		t.Fatalf("a deploy that moved no DSL changed the person: %v", writes)
	}
}

func TestADeployThatDoesNotMoveTheDslFillsAnEmptyRecord(t *testing.T) {
	tree := validPackage()
	h := newHarness(t, tree, ownerPackage())
	h.stager.active = activeSetMatching(t, h, tree, "acme")
	if _, err := deployConfirmed(h, actorNamed("alice")); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if writes := deployerWrites(h); len(writes) != 1 || writes[0] != `"alice"` {
		t.Fatalf("a source staged before the field got %v, want its deployer", writes)
	}
}

// The last person did not consent to DSL they never saw.
func TestStagingByNoPersonBlanksTheRecord(t *testing.T) {
	h := newHarness(t, validPackage(), packageDeployedBy("bob"))
	if _, err := deployConfirmed(h, actorNamed("system:automation:x")); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if writes := deployerWrites(h); len(writes) != 1 || writes[0] != `""` {
		t.Fatalf("staging by no person recorded %v, want it blanked", writes)
	}
}

func TestARunThatShipsNoDslRecordsNobody(t *testing.T) {
	tree := validPackage()
	delete(tree, "dsl/acme/concepts.memql")
	delete(tree, "dsl/acme/memql.toml")
	h := newHarness(t, tree, ownerPackage())
	if _, err := deployConfirmed(h, actorNamed("alice")); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if writes := deployerWrites(h); len(writes) != 0 {
		t.Fatalf("a run with no DSL recorded %v", writes)
	}
}

func TestARunThatPublishesOnlyCandidatesRecordsNobody(t *testing.T) {
	tree := validPackage()
	h := newHarness(t, tree, ownerPackage())
	h.stager.active = activeSetMatching(t, h, tree, "acme")
	placements := firstDeployPlacements()
	for name, p := range placements {
		if name == "storefront" {
			p.Skip = true // a storefront has no candidate version
		} else {
			p.Target = edge.TargetCandidate
		}
		placements[name] = p
	}
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc", Actor: actorNamed("alice"), Confirmed: true, Placements: placements,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if writes := deployerWrites(h); len(writes) != 0 {
		t.Fatalf("a candidate-only run recorded %v", writes)
	}
}

func TestARollbackThatRestagesRecordsItsActor(t *testing.T) {
	prior := map[string]any{
		"id":         "v1:platform:packageDeployment:old",
		"packageId":  "v1:platform:package:abc",
		"status":     StatusSucceeded,
		"dslVersion": "packages/acme/0123456789abcdef/",
		"report":     map[string]any{"dslDomains": []any{map[string]any{"domain": "acme", "namespaces": []any{"acme"}}}},
		"deployables": []any{
			map[string]any{"name": "docs", "siteId": "v1:platform:site:s2", "bundleRef": "blob://sites/s2/v1/"},
		},
	}
	rollback := func(h *harness) {
		t.Helper()
		if _, err := Rollback(context.Background(), h.deps, RollbackRequest{
			PackageId: "v1:platform:package:abc", DeploymentId: "v1:platform:packageDeployment:old", Actor: actorNamed("alice"),
		}); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}

	t.Run("one that puts the DSL back names its actor", func(t *testing.T) {
		h := newHarness(t, validPackage(), packageDeployedBy("bob"))
		h.engine.rows["query packageDeploymentById"] = []map[string]any{prior}
		h.stager.active = map[string]string{"acme": "packages/acme/ffffffffffffffff/"}
		rollback(h)
		if writes := deployerWrites(h); len(writes) != 1 || writes[0] != `"alice"` {
			t.Fatalf("the rollback recorded %v, want its actor", writes)
		}
	})

	t.Run("one that finds the DSL already active moves nothing", func(t *testing.T) {
		h := newHarness(t, validPackage(), packageDeployedBy("bob"))
		h.engine.rows["query packageDeploymentById"] = []map[string]any{prior}
		h.stager.active = map[string]string{"acme": "packages/acme/0123456789abcdef/"}
		rollback(h)
		if writes := deployerWrites(h); len(writes) != 0 {
			t.Fatalf("a rollback that restaged nothing recorded %v", writes)
		}
	})
}

func TestAnalysisNamesTheWorkflowsThatWillUseTheDeployersFleet(t *testing.T) {
	flagged := func(name string) string {
		return "@usesDeployerFleet\n@trigger(schedule=\"0 0 * * * *\")\nautomation " + name + " { return true }\n"
	}
	tree := validPackage()
	tree["dsl/acme/automations.memql"] = file(flagged("zebraBrief") + "\n" + flagged("alphaBrief") +
		"\n@trigger(schedule=\"0 0 * * * *\")\nautomation plainTick { return true }\n")
	dslTree, err := fs.Sub(tree, DslRoot)
	if err != nil {
		t.Fatal(err)
	}
	got := countConstructs(dslTree)["acme"].fleetWorkflows
	if len(got) != 2 || got[0] != "alphaBrief" || got[1] != "zebraBrief" {
		t.Fatalf("the report names %v", got)
	}
}
