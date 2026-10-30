package packages

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// dsl_domains_deploy_test.go -- WHEN package.dslDomains is written (design
// record 2026-10-29, D5; the automated security review of Task 9).
//
// The list is what the bff compares a gate's domain with on every call, so a
// write that precedes a commitment is a widening. Written at analysis it was
// one: a run that was refused, or parked at the confirm gate, or cancelled,
// would already have changed which domains a package owns. It is written by a
// deploy, once the DSL that deploy carries is staged and rolled and before any
// site is published, from the run's own report; and by a rollback, from the
// deployment it returns to. `declares` stays at analysis.

// domainWrites is the dslDomains literal of every recordPackageDslDomains
// statement a harness saw, in order.
func domainWrites(h *harness) []string {
	var out []string
	for _, q := range h.engine.statements() {
		if !strings.HasPrefix(q, "mutation recordPackageDslDomains(") {
			continue
		}
		_, after, _ := strings.Cut(q, "dslDomains: ")
		out = append(out, strings.TrimSuffix(after, ")"))
	}
	return out
}

// deployConfirmed runs a confirmed deploy of the harness's tree, the way a
// person's click does.
func deployConfirmed(h *harness, actor Actor) (*DeployOutcome, error) {
	return Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      actor,
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	})
}

func withBetaDomain(tree fstest.MapFS) fstest.MapFS {
	tree["dsl/beta/concepts.memql"] = file(validConcepts)
	tree["dsl/beta/memql.toml"] = file(languageLine)
	return tree
}

// buildHook runs a function when a build starts, then builds: the place a test
// can reach between the confirm gate and the roll.
type buildHook struct {
	Builder
	onBuild func()
}

func (b buildHook) Build(ctx context.Context, run BuildRun, snapshot *SourceSnapshot, dep DeployableReport) (BuildResult, error) {
	b.onBuild()
	return b.Builder.Build(ctx, run, snapshot, dep)
}

func TestARunThatCommitsNothingLeavesTheDomainsAlone(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T) *harness
	}{
		{"a run parked at the confirm gate", func(t *testing.T) *harness {
			h := newHarness(t, validPackage(), ownerPackage())
			out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "v1:platform:package:abc", Actor: mayDeployDsl()})
			if err != nil || !out.AwaitingConfirm {
				t.Fatalf("setup: want a parked run, got %+v %v", out, err)
			}
			return h
		}},
		{"a run the analysis refuses", func(t *testing.T) *harness {
			h := newHarness(t, bootRefusingPackage(), ownerPackage())
			if _, err := deployConfirmed(h, mayDeployDsl()); RefusalCode(err) != CodeDslRefusesBoot {
				t.Fatalf("setup: want %s, got %v", CodeDslRefusesBoot, err)
			}
			return h
		}},
		{"a run whose gate could never open", func(t *testing.T) *harness {
			h := newHarness(t, gatedPackage(gateOnAnotherProduct), ownerPackage())
			if _, err := deployConfirmed(h, mayDeployDsl()); RefusalCode(err) != CodeDslAccessGateForeignDeployable {
				t.Fatalf("setup: want %s, got %v", CodeDslAccessGateForeignDeployable, err)
			}
			return h
		}},
		{"a run refused for want of authority", func(t *testing.T) *harness {
			h := newHarness(t, validPackage(), ownerPackage())
			if _, err := deployConfirmed(h, plainUser()); RefusalCode(err) != CodeDslRequiresAuthoring {
				t.Fatalf("setup: want %s, got %v", CodeDslRequiresAuthoring, err)
			}
			return h
		}},
		{"a run whose build fails", func(t *testing.T) *harness {
			h := newHarness(t, validPackage(), ownerPackage())
			h.builder.err = errors.New("npm exploded")
			if _, err := deployConfirmed(h, mayDeployDsl()); err == nil {
				t.Fatal("setup: the build was meant to fail")
			}
			return h
		}},
		// CANCELLED AT THE LAST CHECKPOINT, immediately before the roll: an
		// automatic run is stopped by switching its source's auto-deploy off,
		// which the checkpoint reads like a person's Cancel.
		{"a run cancelled before the roll", func(t *testing.T) *harness {
			pkg := ownerPackage()
			pkg["autoDeploy"] = true
			h := newHarness(t, validPackage(), pkg)
			h.deps.Builder = buildHook{Builder: h.builder, onBuild: func() { pkg["autoDeploy"] = false }}
			_, err := Deploy(context.Background(), h.deps, DeployRequest{
				PackageId: "v1:platform:package:abc", Actor: mayDeployDsl(), Automatic: true, Confirmed: true,
				Placements: firstDeployPlacements(),
			})
			if RefusalCode(err) != CodeDeploymentCancelled {
				t.Fatalf("setup: want %s, got %v", CodeDeploymentCancelled, err)
			}
			if h.roller.rolls != 0 {
				t.Fatalf("setup: the cancelled run rolled the cluster")
			}
			return h
		}},
		{"a run whose roll fails", func(t *testing.T) *harness {
			h := newHarness(t, validPackage(), ownerPackage())
			h.roller.err = errors.New("rollout surface down")
			if _, err := deployConfirmed(h, mayDeployDsl()); err == nil {
				t.Fatal("setup: the roll was meant to fail")
			}
			return h
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.run(t)
			if writes := domainWrites(h); len(writes) != 0 {
				t.Fatalf("the list was written %v by %s, which committed nothing", writes, tc.name)
			}
			// Nothing was published either: the list is not what stopped them.
			if len(h.publisher.published) != 0 {
				t.Fatalf("%s published %v", tc.name, h.publisher.published)
			}
		})
	}

	// The control that makes the silence above mean something: the same tree,
	// confirmed by somebody who may deploy it, does write.
	h := newHarness(t, validPackage(), ownerPackage())
	if _, err := deployConfirmed(h, mayDeployDsl()); err != nil {
		t.Fatalf("control failed: %v", err)
	}
	if writes := domainWrites(h); len(writes) != 1 {
		t.Fatalf("control failed: a successful run wrote %v", writes)
	}
}

// What the analysis writes is what the SOURCE declares, and nothing of what it
// ships: declares stays where it was, at the moment the tree was read.
func TestAnAnalysisWritesWhatTheSourceDeclaresAndNotWhatItShips(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "v1:platform:package:abc", Actor: mayDeployDsl()}); err != nil {
		t.Fatal(err)
	}
	stmt := statementContaining(h.engine.statements(), "mutation recordPackageDeployables(")
	if stmt == "" {
		t.Fatal("the analysis no longer records what the source declares")
	}
	if strings.Contains(stmt, "dslDomains") {
		t.Fatalf("the analysis's write carries the domains the package ships: %s", stmt)
	}
	if writes := domainWrites(h); len(writes) != 0 {
		t.Fatalf("an analysis wrote the list: %v", writes)
	}
}

func TestASuccessfulRunWritesTheDomainsOnceTheDslIsLiveAndBeforeAnySitePublishes(t *testing.T) {
	h := newHarness(t, withBetaDomain(validPackage()), ownerPackage())
	atPointer, atRoll := -1, -1
	h.stager.onWrite = func() { atPointer = len(h.engine.statements()) }
	h.roller.onRoll = func() { atRoll = len(h.engine.statements()) }

	if _, err := deployConfirmed(h, mayDeployDsl()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if atRoll < 0 || atPointer < 0 || h.roller.rolls != 1 {
		t.Fatalf("setup: the DSL was not staged and rolled (pointer %d, roll %d, rolls %d)", atPointer, atRoll, h.roller.rolls)
	}

	writes := domainWrites(h)
	if len(writes) != 1 || writes[0] != `["acme","beta"]` {
		t.Fatalf("the run wrote %v, want one write of [\"acme\",\"beta\"] from its own report", writes)
	}
	stmts := h.engine.statements()
	wrote := statementIndex(stmts, "mutation recordPackageDslDomains(")
	publishing := statementIndex(stmts, `status: "publishing"`)
	// After the roll: the list is only ever one whose DSL is live.
	if wrote < atRoll {
		t.Fatalf("the list was written at statement %d, before the roll at %d", wrote, atRoll)
	}
	// Before the publish: the window between the two fails closed.
	if publishing < 0 || wrote > publishing {
		t.Fatalf("the list was written at statement %d, after the run began publishing at %d", wrote, publishing)
	}
	if len(h.publisher.published) == 0 {
		t.Fatal("setup: nothing was published")
	}
	// The record is the package's, addressed by the package.
	if !strings.Contains(stmts[wrote], `packageId: "v1:platform:package:abc"`) {
		t.Fatalf("the write is not about the package: %s", stmts[wrote])
	}
}

// "A package with no DSL change that still deploys writes the same list": a
// source deployed before the field existed has none, and its next deploy is
// what gives it one, whether or not the DSL moved.
func TestADeployWhoseDslIsUnchangedStillWritesTheSameList(t *testing.T) {
	tree := validPackage()
	h := newHarness(t, tree, ownerPackage())
	h.stager.active = activeSetMatching(t, h, tree, "acme")

	if _, err := deployConfirmed(h, mayDeployDsl()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if h.roller.rolls != 0 || h.stager.written != 0 {
		t.Fatalf("setup: the DSL was meant to be unchanged (rolls %d, pointer writes %d)", h.roller.rolls, h.stager.written)
	}
	if writes := domainWrites(h); len(writes) != 1 || writes[0] != `["acme"]` {
		t.Fatalf("an unchanged-DSL deploy wrote %v, want [\"acme\"]", writes)
	}
}

// ...and a package with no DSL at all writes an empty list, which is how a
// source that stopped shipping DSL closes the gates of what it used to ship.
func TestARunThatShipsNoDslWritesAnEmptyList(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	if _, err := deployConfirmed(h, plainUser()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if writes := domainWrites(h); len(writes) != 1 || writes[0] != `[]` {
		t.Fatalf("a no-DSL deploy wrote %v, want [] (an array, never null)", writes)
	}
	stmts := h.engine.statements()
	if wrote, publishing := statementIndex(stmts, "mutation recordPackageDslDomains("), statementIndex(stmts, `status: "publishing"`); wrote < 0 || wrote > publishing {
		t.Fatalf("the empty list was written at %d, not before publishing at %d", wrote, publishing)
	}
}

// REWRITTEN WHOLE, from the run's own report: a domain dropped upstream is not
// in the list the next deploy writes.
func TestADomainDroppedUpstreamLeavesTheListOnTheNextDeploy(t *testing.T) {
	first := newHarness(t, withBetaDomain(validPackage()), ownerPackage())
	if _, err := deployConfirmed(first, mayDeployDsl()); err != nil {
		t.Fatal(err)
	}
	second := newHarness(t, validPackage(), ownerPackage())
	if _, err := deployConfirmed(second, mayDeployDsl()); err != nil {
		t.Fatal(err)
	}
	a, b := domainWrites(first), domainWrites(second)
	if len(a) != 1 || a[0] != `["acme","beta"]` || len(b) != 1 || b[0] != `["acme"]` {
		t.Fatalf("the lists went from %v to %v, want [\"acme\",\"beta\"] and then [\"acme\"]", a, b)
	}
}

// A write that cannot land does not stop a deploy: it is bookkeeping on the
// package row, as declares and the deployed version are, and a historic
// package with no organization refuses every such write. What it leaves is the
// previous list, which can close a gate and opens none the live tree lacks.
func TestAListThatCannotBeWrittenDoesNotStopTheDeploy(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.fail = map[string]error{"mutation recordPackageDslDomains(": errors.New("organization_required: attribute this existing record before changing it")}
	out, err := deployConfirmed(h, mayDeployDsl())
	if err != nil || out.Status != StatusSucceeded {
		t.Fatalf("a failed list write stopped the deploy: %+v %v", out, err)
	}
	if len(h.publisher.published) == 0 {
		t.Fatal("the sites were not published")
	}
}

// A ROLLBACK REWRITES THE LIST FROM THE DEPLOYMENT IT RETURNS TO, after the DSL
// is back, using the report that deployment recorded.
func TestARollbackRestoresTheDomainsOfTheDeploymentItReturnsTo(t *testing.T) {
	priorWith := func(report map[string]any) map[string]any {
		row := map[string]any{
			"id":         "v1:platform:packageDeployment:old",
			"packageId":  "v1:platform:package:abc",
			"status":     StatusSucceeded,
			"dslVersion": "packages/acme/0123456789abcdef/",
			"deployables": []any{
				map[string]any{"name": "docs", "siteId": "v1:platform:site:s2", "bundleRef": "blob://sites/s2/v1/"},
			},
		}
		if report != nil {
			row["report"] = report
		}
		return row
	}
	rollback := func(h *harness, actor Actor) error {
		_, err := Rollback(context.Background(), h.deps, RollbackRequest{
			PackageId:    "v1:platform:package:abc",
			DeploymentId: "v1:platform:packageDeployment:old",
			Actor:        actor,
		})
		return err
	}
	recorded := map[string]any{"dslDomains": []any{
		map[string]any{"domain": "acme", "namespaces": []any{"acme"}},
		map[string]any{"domain": "betadir", "namespaces": []any{"realns"}},
	}}

	t.Run("the list is the one that deployment recorded, written after the roll", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		// The cluster is on a newer prefix, so the pointer has to move and roll.
		h.stager.active = map[string]string{"acme": "packages/acme/ffffffffffffffff/"}
		atRoll := -1
		h.roller.onRoll = func() { atRoll = len(h.engine.statements()) }

		if err := rollback(h, mayDeployDsl()); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if h.roller.rolls != 1 || atRoll < 0 {
			t.Fatalf("setup: the rollback did not roll")
		}
		writes := domainWrites(h)
		if len(writes) != 1 || writes[0] != `["acme","realns"]` {
			t.Fatalf("the rollback wrote %v, want [\"acme\",\"realns\"] from the deployment it returned to", writes)
		}
		if wrote := statementIndex(h.engine.statements(), "mutation recordPackageDslDomains("); wrote < atRoll {
			t.Fatalf("the list was written at statement %d, before the DSL was rolled back at %d", wrote, atRoll)
		}
	})

	// A deployment from before the field recorded no namespaces, so there is no
	// list to restore, and the honest one is empty: closed until a deploy.
	t.Run("a deployment recorded before the field restores an empty list", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(map[string]any{
			"dslDomains": []any{map[string]any{"domain": "acme"}},
		})}
		if err := rollback(h, mayDeployDsl()); err != nil {
			t.Fatal(err)
		}
		if writes := domainWrites(h); len(writes) != 1 || writes[0] != `[]` {
			t.Fatalf("the rollback wrote %v, want [] (nothing was recorded to restore)", writes)
		}
	})

	t.Run("a rollback that does not complete writes nothing", func(t *testing.T) {
		refused := newHarness(t, validPackage(), ownerPackage())
		refused.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		if err := rollback(refused, plainUser()); RefusalCode(err) != CodeDslRequiresAuthoring {
			t.Fatalf("setup: want %s, got %v", CodeDslRequiresAuthoring, err)
		}
		failed := newHarness(t, validPackage(), ownerPackage())
		failed.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		failed.stager.active = map[string]string{"acme": "packages/acme/ffffffffffffffff/"}
		failed.roller.err = errors.New("rollout surface down")
		if err := rollback(failed, mayDeployDsl()); err == nil {
			t.Fatal("setup: the roll was meant to fail")
		}
		for name, h := range map[string]*harness{"a refused rollback": refused, "a rollback whose roll failed": failed} {
			if writes := domainWrites(h); len(writes) != 0 {
				t.Fatalf("%s wrote the list %v", name, writes)
			}
		}
	})

	t.Run("a deployment that never went live restores nothing", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		row := priorWith(recorded)
		row["status"] = StatusRefused
		h.engine.rows["query packageDeploymentById"] = []map[string]any{row}
		if err := rollback(h, mayDeployDsl()); err == nil {
			t.Fatal("setup: a refused deployment was rolled back to")
		}
		if writes := domainWrites(h); len(writes) != 0 {
			t.Fatalf("wrote %v", writes)
		}
	})
}
