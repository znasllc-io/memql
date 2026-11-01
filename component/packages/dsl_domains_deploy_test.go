package packages

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/edge"
)

// dsl_domains_deploy_test.go -- WHEN package.dslDomains is written (design
// record 2026-10-29, D5; the automated security review of Task 9).
//
// The list is what the bff compares a gate's domain with on every call, so a
// write that precedes a commitment is a widening. Written at analysis it was
// one: a run that was refused, or parked at the confirm gate, or cancelled,
// would already have changed which domains a package owns. It is written by a
// deploy, once the DSL that deploy carries is staged and its roll requested and
// before any site is published, from the run's own report -- and only by a run
// that changes what a visitor is served: one that publishes a serving version,
// or publishes nothing and carries DSL. A run that publishes only candidates
// leaves it. A rollback writes it from the deployment it returns to.
// `declares` stays at analysis.

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

// A RUN THAT PUBLISHES NO SERVING VERSION LEAVES THE LIST ALONE.
//
// The list is what every visitor's access gate is compared with, and a
// candidate run leaves every visitor on the serving build
// (docs/public/operate/deployables.md). Writing the list from a tree no visitor
// is being served lets that tree answer for the gates of the build visitors ARE
// being served. Measured through the real Deploy before this was decided: a candidate
// of a tree with no DSL wrote [] and closed every access code on the site; a
// candidate of a tree that dropped a domain wrote the shorter list with no roll,
// no pointer write and the domain still active; and a run with every app
// skipped on a tree with no DSL wrote [] having published nothing.
//
// EVERY CASE IS PAIRED WITH THE SAME TREE PUBLISHED TO VISITORS, so what differs
// between "writes" and "leaves alone" is only whether a visitor is about to be
// served this run. The one run that publishes nothing and still writes is the
// one refuseCandidateDslChange sends a person to -- every app skipped, on a tree
// that really carries DSL -- because staging and rolling that DSL is the deploy.
func TestARunThatPublishesNoServingVersionLeavesTheListAlone(t *testing.T) {
	skipBoth := map[string]Placement{"storefront": {Skip: true}, "docs": {Skip: true}}
	servingDocs := map[string]Placement{"storefront": {Skip: true}, "docs": {Hostname: "docs.example.com"}}
	servingAndCandidate := map[string]Placement{
		"storefront": {Hostname: "shop.example.com"},
		"docs":       {Hostname: "docs.example.com", Target: edge.TargetCandidate},
	}

	// What the cluster is running when each run starts.
	nothingLive := func(*testing.T, *harness, fstest.MapFS) map[string]string { return map[string]string{} }
	acmeLive := func(t *testing.T, h *harness, tree fstest.MapFS) map[string]string {
		return activeSetMatching(t, h, tree, "acme")
	}
	acmeAndBetaLive := func(t *testing.T, h *harness, tree fstest.MapFS) map[string]string {
		active := activeSetMatching(t, h, tree, "acme")
		active["beta"] = "packages/beta/ffffffffffffffff/"
		return active
	}
	staleAcme := func(*testing.T, *harness, fstest.MapFS) map[string]string {
		return map[string]string{"acme": "packages/acme/ffffffffffffffff/"}
	}

	for _, tc := range []struct {
		name       string
		tree       fstest.MapFS
		active     func(*testing.T, *harness, fstest.MapFS) map[string]string
		placements map[string]Placement
		// want is every list write, in order; nil is none.
		want    []string
		rolls   int
		targets []edge.Target
	}{
		{
			name: "a candidate of a tree with no DSL", tree: spaOnlyPackage(), active: staleAcme,
			placements: candidatePlacements(), want: nil, targets: []edge.Target{edge.TargetCandidate},
		},
		{
			name: "a serving publish of a tree with no DSL", tree: spaOnlyPackage(), active: staleAcme,
			placements: servingDocs, want: []string{`[]`}, targets: []edge.Target{edge.TargetServing},
		},
		{
			name: "a candidate of a tree that dropped a domain", tree: validPackage(), active: acmeAndBetaLive,
			placements: candidatePlacements(), want: nil, targets: []edge.Target{edge.TargetCandidate},
		},
		{
			name: "a serving publish of a tree that dropped a domain", tree: validPackage(), active: acmeAndBetaLive,
			placements: servingDocs, want: []string{`["acme"]`}, targets: []edge.Target{edge.TargetServing},
		},
		{
			name: "a candidate of a tree whose DSL is what the cluster runs", tree: validPackage(), active: acmeLive,
			placements: candidatePlacements(), want: nil, targets: []edge.Target{edge.TargetCandidate},
		},
		{
			name: "a serving publish of a tree whose DSL is what the cluster runs", tree: validPackage(), active: acmeLive,
			placements: servingDocs, want: []string{`["acme"]`}, targets: []edge.Target{edge.TargetServing},
		},
		{
			name: "every app skipped on a tree with no DSL", tree: spaOnlyPackage(), active: nothingLive,
			placements: skipBoth, want: nil,
		},
		// THE EXCEPTION, and the control that makes the silence above mean
		// something: nothing is published and the list is still written, because
		// the DSL this run stages and rolls is the whole of its deploy.
		{
			name: "every app skipped on a tree that carries DSL", tree: validPackage(), active: nothingLive,
			placements: skipBoth, want: []string{`["acme"]`}, rolls: 1,
		},
		// A candidate beside a serving version is a run that serves.
		{
			name: "a candidate beside a serving version", tree: validPackage(), active: acmeLive,
			placements: servingAndCandidate, want: []string{`["acme"]`},
			targets: []edge.Target{edge.TargetServing, edge.TargetCandidate},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.tree, ownerPackage())
			h.stager.active = tc.active(t, h, tc.tree)
			before := map[string]string{}
			for k, v := range h.stager.active {
				before[k] = v
			}
			if _, err := Deploy(context.Background(), h.deps, DeployRequest{
				PackageId: "v1:platform:package:abc", Actor: mayDeployDsl(), Confirmed: true, Placements: tc.placements,
			}); err != nil {
				t.Fatalf("deploy: %v", err)
			}

			// The setup is the one the case names, not a different run.
			if !reflect.DeepEqual(h.publisher.targets, tc.targets) {
				t.Fatalf("the run published %v, want %v", h.publisher.targets, tc.targets)
			}
			if h.roller.rolls != tc.rolls {
				t.Fatalf("the run rolled %d times, want %d", h.roller.rolls, tc.rolls)
			}
			if tc.rolls == 0 && (h.stager.written != 0 || !reflect.DeepEqual(h.stager.active, before)) {
				t.Fatalf("a run that rolled nothing moved the pointer: %v -> %v", before, h.stager.active)
			}

			if got := domainWrites(h); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the run wrote the list %v, want %v", got, tc.want)
			}
		})
	}
}

// writesDslDomains answers from the plan what the publish stage will do, so it
// has to agree with publish on the cases a whole Deploy cannot reach: the build
// stage never hands a skipped app a bundle, and a deployable with a non-fatal
// problem has none either.
func TestWritesDslDomainsAgreesWithWhatThePublishStagePublishes(t *testing.T) {
	deployables := func(names ...string) []DeployableReport {
		out := make([]DeployableReport, 0, len(names))
		for _, n := range names {
			out = append(out, DeployableReport{Name: n})
		}
		return out
	}
	withDsl := []DslDomainReport{{Domain: "acme"}}
	built := func(names ...string) map[string]edge.Bundle {
		out := map[string]edge.Bundle{}
		for _, n := range names {
			out[n] = edge.Bundle{"index.html": []byte("<!doctype html>")}
		}
		return out
	}
	candidate := Placement{Target: edge.TargetCandidate}

	for _, tc := range []struct {
		name       string
		rep        *Report
		placements map[string]Placement
		bundles    map[string]edge.Bundle
		want       bool
	}{
		{"no report", nil, nil, nil, false},
		{"a package of DSL alone", &Report{DslDomains: withDsl}, nil, nil, true},
		{"a serving app", &Report{Deployables: deployables("web")}, nil, built("web"), true},
		{"an explicit serving target", &Report{Deployables: deployables("web")},
			map[string]Placement{"web": {Target: edge.TargetServing}}, built("web"), true},
		{"a lone candidate", &Report{Deployables: deployables("web")},
			map[string]Placement{"web": candidate}, built("web"), false},
		{"a lone candidate of a package that carries DSL", &Report{Deployables: deployables("web"), DslDomains: withDsl},
			map[string]Placement{"web": candidate}, built("web"), false},
		{"a candidate beside a serving app", &Report{Deployables: deployables("web", "docs")},
			map[string]Placement{"web": candidate}, built("web", "docs"), true},
		// A skipped app publishes nothing even if a bundle for it exists, so it is
		// neither a serving version nor a candidate.
		{"a skipped app that has a bundle", &Report{Deployables: deployables("web")},
			map[string]Placement{"web": {Skip: true}}, built("web"), false},
		{"a skipped app on a package that carries DSL", &Report{Deployables: deployables("web"), DslDomains: withDsl},
			map[string]Placement{"web": {Skip: true}}, built("web"), true},
		// A skipped CANDIDATE is not a candidate: it publishes nothing, so the
		// DSL this run carries is still the whole of its deploy.
		{"a skipped candidate on a package that carries DSL", &Report{Deployables: deployables("web"), DslDomains: withDsl},
			map[string]Placement{"web": {Skip: true, Target: edge.TargetCandidate}}, built("web"), true},
		{"an app with no bundle", &Report{Deployables: deployables("web")}, nil, built(), false},
		{"an app with no bundle on a package that carries DSL", &Report{Deployables: deployables("web"), DslDomains: withDsl}, nil, built(), true},
		// An app with no bundle does not turn a candidate beside it into a run
		// that publishes nothing.
		{"a candidate beside an app with no bundle", &Report{Deployables: deployables("web", "docs"), DslDomains: withDsl},
			map[string]Placement{"web": candidate}, built("web"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := writesDslDomains(tc.rep, tc.placements, tc.bundles); got != tc.want {
				t.Fatalf("writesDslDomains = %v, want %v", got, tc.want)
			}
		})
	}
}

// A PARKED RUN IS NOT A COMMITMENT AND ITS CONFIRMATION IS (memql#4954). The
// gate is the first call and the confirm the second, so the write that follows a
// commitment belongs to the second and happens once.
func TestAParkedRunWritesTheListWhenItIsConfirmedAndNotBefore(t *testing.T) {
	t.Run("a run that serves", func(t *testing.T) {
		h := newHarness(t, withBetaDomain(validPackage()), ownerPackage())
		parked, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "v1:platform:package:abc", Actor: mayDeployDsl()})
		if err != nil || !parked.AwaitingConfirm {
			t.Fatalf("setup: want a run parked at the gate, got %+v %v", parked, err)
		}
		if writes := domainWrites(h); len(writes) != 0 || h.roller.rolls != 0 {
			t.Fatalf("the park wrote %v and rolled %d times before anyone confirmed", writes, h.roller.rolls)
		}

		h.engine.rows["query packageDeploymentById"] = []map[string]any{parkedRun(parked.DeploymentId, "v1:platform:package:abc", nil)}
		confirmed, err := Deploy(context.Background(), h.deps, DeployRequest{
			PackageId: "v1:platform:package:abc", Actor: mayDeployDsl(), Confirmed: true,
			DeploymentId: parked.DeploymentId, Placements: firstDeployPlacements(),
		})
		if err != nil || confirmed.Status != StatusSucceeded || confirmed.DeploymentId != parked.DeploymentId {
			t.Fatalf("the confirmation did not complete the run it confirmed: %+v %v", confirmed, err)
		}
		// Once, from the report of the confirming call's own analysis.
		if writes := domainWrites(h); len(writes) != 1 || writes[0] != `["acme","beta"]` {
			t.Fatalf("the confirmed run wrote %v, want one write of [\"acme\",\"beta\"]", writes)
		}
	})

	// THE TARGET A RUN WAS OPENED WITH SURVIVES THE GATE, and so does what it
	// means for the list: a run opened as a candidate and confirmed by a call
	// that left the target out is still a candidate, and still leaves the list.
	t.Run("a candidate confirmed without naming its target", func(t *testing.T) {
		h := confirmParked(t, []any{"docs"}, map[string]Placement{
			"storefront": {Skip: true},
			"docs":       {Hostname: "docs.example.com"},
		})
		if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetCandidate {
			t.Fatalf("setup: the confirm published %v, want the recorded candidate", got)
		}
		if writes := domainWrites(h); len(writes) != 0 {
			t.Fatalf("a confirmed candidate wrote the list %v", writes)
		}
	})

	t.Run("a candidate the person chose to serve at the gate", func(t *testing.T) {
		h := confirmParked(t, []any{"docs"}, map[string]Placement{
			"storefront": {Skip: true},
			"docs":       {Hostname: "docs.example.com", Target: edge.TargetServing},
		})
		if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetServing {
			t.Fatalf("setup: the confirm published %v, want the serving version", got)
		}
		if writes := domainWrites(h); len(writes) != 1 || writes[0] != `[]` {
			t.Fatalf("a run served at the gate wrote %v, want its one empty list", writes)
		}
	})
}

// logLine is the first line of a captured log containing sub, or "".
func logLine(log fmt.Stringer, sub string) string {
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

// A write that cannot land does not stop a deploy: it is bookkeeping on the
// package row, as declares and the deployed version are, and a historic
// package with no organization refuses every such write. What it leaves is the
// previous list, which can close a gate and opens none the live tree lacks.
//
// AND IT SAYS SO WHERE THE PERSON LOOKING AT THE RUN WILL LOOK: the line is
// stamped with the deployment it is about (logger.Subject), which is what puts
// it in that run's Logs section rather than only in a node's output.
func TestAListThatCannotBeWrittenDoesNotStopTheDeploy(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	log, buf := capturedLog()
	h.deps.Logger = log
	h.engine.fail = map[string]error{"mutation recordPackageDslDomains(": errors.New("organization_required: attribute this existing record before changing it")}
	out, err := deployConfirmed(h, mayDeployDsl())
	if err != nil || out.Status != StatusSucceeded {
		t.Fatalf("a failed list write stopped the deploy: %+v %v", out, err)
	}
	if len(h.publisher.published) == 0 {
		t.Fatal("the sites were not published")
	}
	line := logLine(buf, "could not record which DSL domains")
	if line == "" {
		t.Fatalf("the failed write was not logged:\n%s", buf)
	}
	if !strings.Contains(line, "subject.id="+out.DeploymentId) || !strings.Contains(line, "subject.concept="+packageDeploymentConcept) {
		t.Fatalf("the line is not about the run, so its Logs will not show it: %s", line)
	}
}

// A run that leaves the list alone says so, against the run, so a person who
// reads the Logs of a candidate run and finds no list write knows it was
// decided rather than lost.
func TestARunThatLeavesTheListAloneSaysSoInItsLogs(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	log, buf := capturedLog()
	h.deps.Logger = log
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc", Actor: plainUser(), Confirmed: true, Placements: candidatePlacements(),
	})
	if err != nil {
		t.Fatal(err)
	}
	line := logLine(buf, "publishes no serving version")
	if line == "" {
		t.Fatalf("a candidate run did not say it left the list alone:\n%s", buf)
	}
	if !strings.Contains(line, "subject.id="+out.DeploymentId) || !strings.Contains(line, "subject.concept="+packageDeploymentConcept) {
		t.Fatalf("the line is not about the run: %s", line)
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

	// THE LIST DESCRIBES LIVE DSL, so it is written only when the earlier
	// deployment's DSL is live again: restored now, or found already active. A
	// cluster that cannot stage DSL cannot restore it, and says so the way a
	// deploy does (stageAndRoll's refusal) -- before anything has moved, so the
	// sites and the DSL are not left apart.
	t.Run("a cluster with no staging surface refuses before anything moves", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		h.deps.Stager = nil
		err := rollback(h, mayDeployDsl())
		if RefusalCode(err) != CodeSourceUnreadable || !strings.Contains(err.Error(), "no DSL staging surface") {
			t.Fatalf("want a %s refusal naming the missing staging surface, got %v", CodeSourceUnreadable, err)
		}
		if len(h.publisher.repointed) != 0 || h.roller.rolls != 0 {
			t.Fatalf("the refusal came after something moved: repointed %v, rolls %d", h.publisher.repointed, h.roller.rolls)
		}
		if writes := domainWrites(h); len(writes) != 0 {
			t.Fatalf("a rollback that restored no DSL wrote the list %v", writes)
		}
	})

	// THE OTHER WAY OF HAVING NO SURFACE, and the one production has. The
	// integration always wires the blob stager, so the nil check above never
	// fires on a real node: a cluster with no object storage refuses inside the
	// stager's first read. That read used to come after the sites were
	// repointed, so the refusal arrived with the sites already back and the DSL
	// not -- the apart-state the refusal exists to prevent, under a sentence that
	// promised nothing had changed.
	t.Run("a stager that cannot be read refuses before anything moves", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		h.stager.readErr = refuse(CodeSourceUnreadable, "the active-set pointer is not readable")
		err := rollback(h, mayDeployDsl())
		if RefusalCode(err) != CodeSourceUnreadable {
			t.Fatalf("want a %s refusal, got %v", CodeSourceUnreadable, err)
		}
		if len(h.publisher.repointed) != 0 || h.roller.rolls != 0 || h.stager.written != 0 {
			t.Fatalf("the refusal came after something moved: repointed %v, rolls %d, pointer writes %d",
				h.publisher.repointed, h.roller.rolls, h.stager.written)
		}
		if writes := domainWrites(h); len(writes) != 0 {
			t.Fatalf("a rollback that restored nothing wrote the list %v", writes)
		}
	})

	// The same thing with the stager production builds, on a cluster with no
	// object storage: the case the review measured, not a stand-in for it.
	t.Run("the production stager with no object storage refuses before anything moves", func(t *testing.T) {
		t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "")
		h := newHarness(t, validPackage(), ownerPackage())
		h.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		h.deps.Stager = newBlobStager()
		err := rollback(h, mayDeployDsl())
		if RefusalCode(err) != CodeSourceUnreadable || !strings.Contains(err.Error(), "no object storage configured") {
			t.Fatalf("want a %s refusal naming the missing storage, got %v", CodeSourceUnreadable, err)
		}
		if len(h.publisher.repointed) != 0 || h.roller.rolls != 0 {
			t.Fatalf("the refusal came after something moved: repointed %v, rolls %d", h.publisher.repointed, h.roller.rolls)
		}
		if writes := domainWrites(h); len(writes) != 0 {
			t.Fatalf("a rollback that restored nothing wrote the list %v", writes)
		}
	})

	// The rollback's lines are about the deployment it returns to, and carry it
	// as their subject for the reason the deploy's do: the run's Logs section
	// selects by it.
	t.Run("its log lines are about the deployment it returned to", func(t *testing.T) {
		const returnedTo = "subject.id=v1:platform:packageDeployment:old"
		failed := newHarness(t, validPackage(), ownerPackage())
		failedLog, failedBuf := capturedLog()
		failed.deps.Logger = failedLog
		failed.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		failed.stager.active = map[string]string{"acme": "packages/acme/ffffffffffffffff/"}
		failed.engine.fail = map[string]error{"mutation recordPackageDslDomains(": errors.New("organization_required: attribute this existing record before changing it")}
		if err := rollback(failed, mayDeployDsl()); err != nil {
			t.Fatalf("a list that could not be written stopped the rollback: %v", err)
		}
		warn := logLine(failedBuf, "could not record which DSL domains")
		if warn == "" || !strings.Contains(warn, returnedTo) || !strings.Contains(warn, "subject.concept="+packageDeploymentConcept) {
			t.Fatalf("the failed write is not logged against the deployment: %q\n%s", warn, failedBuf)
		}

		skipped := newHarness(t, spaOnlyPackage(), ownerPackage())
		skippedLog, skippedBuf := capturedLog()
		skipped.deps.Logger = skippedLog
		row := priorWith(map[string]any{"dslDomains": []any{}})
		delete(row, "dslVersion")
		skipped.engine.rows["query packageDeploymentById"] = []map[string]any{row}
		if err := rollback(skipped, plainUser()); err != nil {
			t.Fatal(err)
		}
		info := logLine(skippedBuf, "left as it was")
		if info == "" || !strings.Contains(info, returnedTo) {
			t.Fatalf("the skipped write is not logged against the deployment: %q\n%s", info, skippedBuf)
		}
	})

	t.Run("a DSL found already live is as good as one restored", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.engine.rows["query packageDeploymentById"] = []map[string]any{priorWith(recorded)}
		h.stager.active = map[string]string{"acme": "packages/acme/0123456789abcdef/"}
		if err := rollback(h, mayDeployDsl()); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if h.roller.rolls != 0 || h.stager.written != 0 {
			t.Fatalf("setup: the DSL was already the earlier deployment's, so nothing should have moved (rolls %d, pointer writes %d)", h.roller.rolls, h.stager.written)
		}
		if writes := domainWrites(h); len(writes) != 1 || writes[0] != `["acme","realns"]` {
			t.Fatalf("the rollback wrote %v, want [\"acme\",\"realns\"]: the DSL is live, so its list is too", writes)
		}
	})

	// A deployment that carried no DSL records no version to put back, so a
	// rollback to it leaves the DSL exactly as it is, and the list is the DSL's:
	// writing that deployment's own list over DSL a later deployment added would
	// describe DSL that is not what is live.
	t.Run("a deployment that carried no DSL leaves the DSL and the list as they are", func(t *testing.T) {
		h := newHarness(t, spaOnlyPackage(), ownerPackage())
		row := priorWith(map[string]any{"dslDomains": []any{}})
		delete(row, "dslVersion")
		h.engine.rows["query packageDeploymentById"] = []map[string]any{row}
		h.stager.active = map[string]string{"acme": "packages/acme/ffffffffffffffff/"}
		if err := rollback(h, plainUser()); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if len(h.publisher.repointed) != 1 {
			t.Fatalf("setup: the site was not repointed: %v", h.publisher.repointed)
		}
		if h.roller.rolls != 0 || h.stager.written != 0 {
			t.Fatalf("a rollback with no DSL to restore moved the DSL (rolls %d, pointer writes %d)", h.roller.rolls, h.stager.written)
		}
		if writes := domainWrites(h); len(writes) != 0 {
			t.Fatalf("a rollback that restored no DSL wrote the list %v", writes)
		}
	})

	// ...and the refusal above is about DSL, not about the cluster: a package
	// that ships none rolls back on a cluster with no staging surface.
	t.Run("a deployment that carried no DSL rolls back where nothing can stage DSL", func(t *testing.T) {
		h := newHarness(t, spaOnlyPackage(), ownerPackage())
		row := priorWith(map[string]any{"dslDomains": []any{}})
		delete(row, "dslVersion")
		h.engine.rows["query packageDeploymentById"] = []map[string]any{row}
		h.deps.Stager = nil
		if err := rollback(h, plainUser()); err != nil {
			t.Fatalf("a package with no DSL was refused a rollback for want of a staging surface: %v", err)
		}
		if len(h.publisher.repointed) != 1 {
			t.Fatalf("the site was not repointed: %v", h.publisher.repointed)
		}
	})

	// A recorded version that names no domain restored nothing, and nothing
	// restored is not DSL found live.
	t.Run("a DSL version that names no domain is not DSL that is live", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		row := priorWith(recorded)
		row["dslVersion"] = "not-a-staged-prefix"
		h.engine.rows["query packageDeploymentById"] = []map[string]any{row}
		if err := rollback(h, mayDeployDsl()); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if writes := domainWrites(h); len(writes) != 0 {
			t.Fatalf("a DSL version naming no domain was taken for live DSL: wrote %v", writes)
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
