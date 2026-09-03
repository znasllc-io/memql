package packages

import (
	"strings"
	"testing"
	"testing/fstest"
)

// NOTE ON PARALLELISM: nothing in this file calls t.Parallel(). The DSL gate
// mounts the candidate's domains on the process tree for the width of one
// pass; two passes at once would validate each against the other's domains,
// which is why AnalyzePackageDSL serializes them -- and a parallel test would
// simply queue behind that lock while looking like it had won something.

func TestValidPackageAnalyzesClean(t *testing.T) {
	rep, err := Analyze(validPackage(), Options{SourceVersion: "abc123"})
	if err != nil {
		t.Fatalf("valid package refused: %v (problems: %+v)", err, rep.Problems)
	}
	if !rep.OK {
		t.Fatalf("valid package not OK: %+v", rep.Problems)
	}
	if rep.Name != "acme" || rep.SourceVersion != "abc123" || rep.FormatVersion != 1 {
		t.Fatalf("report header wrong: %+v", rep)
	}

	if len(rep.Deployables) != 2 {
		t.Fatalf("want 2 deployables, got %d: %+v", len(rep.Deployables), rep.Deployables)
	}
	front := rep.Deployables[0]
	if front.Name != "storefront" || front.Kind != KindStorefront {
		t.Fatalf("first deployable wrong: %+v", front)
	}
	if front.Prebuilt {
		t.Fatalf("no dist/ in the fixture, so nothing is prebuilt: %+v", front)
	}
	if front.Command != DefaultBuildCommand || front.Output != DefaultBuildOutput {
		t.Fatalf("build defaults not applied: %+v", front)
	}
	if !strings.Contains(front.BuildPlan, DefaultBuildCommand) {
		t.Fatalf("build plan should name the command: %q", front.BuildPlan)
	}
	if front.Binding == nil || front.Binding.StorefrontTokenRef != "acme-storefront-token" {
		t.Fatalf("storefront binding not carried: %+v", front.Binding)
	}

	// The DSL half: discovered, not declared, and counted.
	if len(rep.DslDomains) != 1 || rep.DslDomains[0].Domain != "acme" {
		t.Fatalf("want one discovered domain 'acme', got %+v", rep.DslDomains)
	}
	if got := rep.DslDomains[0].Constructs["concept"]; got != 1 {
		t.Fatalf("want 1 concept counted in acme, got %d (%+v)", got, rep.DslDomains[0].Constructs)
	}
	if len(rep.GoPacks) != 0 {
		t.Fatalf("no bff/ in the fixture: %+v", rep.GoPacks)
	}
}

// The reachable positive for every "refuses with code X" case below: the same
// analysis, on the same tree minus the one broken fact, passes. Without it an
// assertion that something was refused says nothing about WHY.
func TestEachManifestRuleRefusesWithItsCatalogedCode(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fstest.MapFS)
		want   string
	}{
		{
			name:   "no manifest at all",
			mutate: func(p fstest.MapFS) { delete(p, ManifestName) },
			want:   CodeManifestMissing,
		},
		{
			name:   "unparseable yaml",
			mutate: func(p fstest.MapFS) { p[ManifestName] = file("formatVersion: [1\nname: acme\n") },
			want:   CodeManifestInvalid,
		},
		{
			name:   "unknown formatVersion refuses rather than best-effort parsing",
			mutate: func(p fstest.MapFS) { p[ManifestName] = file("formatVersion: 99\nname: acme\n") },
			want:   CodeManifestInvalid,
		},
		{
			name:   "no formatVersion",
			mutate: func(p fstest.MapFS) { p[ManifestName] = file("name: acme\n") },
			want:   CodeManifestInvalid,
		},
		{
			name:   "no name",
			mutate: func(p fstest.MapFS) { p[ManifestName] = file("formatVersion: 1\n") },
			want:   CodeManifestInvalid,
		},
		{
			name: "a mistyped key is refused, not ignored",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployabels: []\n")
			},
			want: CodeManifestInvalid,
		},
		{
			name: "two deployables sharing a name",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n" +
					"  - {name: w, path: clients/web, kind: spa}\n" +
					"  - {name: w, path: clients/docs, kind: static}\n")
			},
			want: CodeManifestInvalid,
		},
		{
			name: "a declared path that is not a directory",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n" +
					"  - {name: w, path: clients/nope, kind: spa}\n")
			},
			want: CodeDeployablePathMissing,
		},
		{
			name: "a path that is a file, not a directory",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n" +
					"  - {name: w, path: clients/docs/index.html, kind: spa}\n")
			},
			want: CodeDeployablePathMissing,
		},
		{
			name: "an unknown kind",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n" +
					"  - {name: w, path: clients/web, kind: server}\n")
			},
			want: CodeDeployableKindUnknown,
		},
		{
			// The third of the target model's three cases (design section
			// B): a kind nobody has heard of stays FATAL and unknown. The
			// second case -- a kind the model knows and does not offer -- is
			// TestAKnownUnofferedKindIsReportedAndTheRestStillDeploys, and
			// the two must not collapse: reading `banana` as "not offered"
			// would tell an author their typo is a platform roadmap item.
			name: "a kind nobody has heard of",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n" +
					"  - {name: w, path: clients/web, kind: banana}\n")
			},
			want: CodeDeployableKindUnknown,
		},
		{
			name: "a storefront with no binding",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n" +
					"  - {name: w, path: clients/web, kind: shopify_storefront}\n")
			},
			want: CodeDeployableBindingMissing,
		},
		{
			name: "a storefront with half a binding",
			mutate: func(p fstest.MapFS) {
				p[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n" +
					"  - name: w\n    path: clients/web\n    kind: shopify_storefront\n" +
					"    binding: {storeDomain: acme.myshopify.com}\n")
			},
			want: CodeDeployableBindingMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPackage()
			// The reachable positive: this tree analyzes clean before the
			// mutation, so a refusal after it is attributable to the mutation.
			if _, err := Analyze(p, Options{}); err != nil {
				t.Fatalf("control failed: the unmutated fixture must analyze clean, got %v", err)
			}
			tc.mutate(p)
			rep, err := Analyze(p, Options{})
			if err == nil {
				t.Fatalf("want refusal %s, got a clean report: %+v", tc.want, rep)
			}
			if got := RefusalCode(err); got != tc.want {
				t.Fatalf("want code %s, got %s (%v)", tc.want, got, err)
			}
			if rep.OK {
				t.Fatalf("a refused package must not report OK: %+v", rep.Problems)
			}
		})
	}
}

func TestPrebuiltOutputSkipsTheBuild(t *testing.T) {
	rep, err := Analyze(prebuiltPackage(), Options{})
	if err != nil {
		t.Fatalf("prebuilt package refused: %v", err)
	}
	var front *DeployableReport
	for i := range rep.Deployables {
		if rep.Deployables[i].Name == "storefront" {
			front = &rep.Deployables[i]
		}
	}
	if front == nil {
		t.Fatal("storefront missing from the report")
	}
	if !front.Prebuilt {
		t.Fatalf("dist/index.html present, so the build must be skipped: %+v", front)
	}
	if !strings.Contains(front.BuildPlan, "build skipped") {
		t.Fatalf("build plan must say so: %q", front.BuildPlan)
	}
	// The reachable positive for the flag: the same deployable in the tree
	// WITHOUT dist/ is not prebuilt, which TestValidPackageAnalyzesClean pins.
	// And the OTHER deployable here still builds, so "prebuilt" is per
	// deployable rather than per package.
	for _, d := range rep.Deployables {
		if d.Name == "docs" && d.Prebuilt {
			t.Fatalf("docs has no dist/, so it must not be prebuilt: %+v", d)
		}
	}
}

func TestGoPackIsReportedAndTheRestStillDeploys(t *testing.T) {
	rep, err := Analyze(goPackPackage(), Options{})
	if err != nil {
		t.Fatalf("a Go pack must not refuse the package: %v", err)
	}
	if !rep.OK {
		t.Fatalf("the rest of the package must still deploy: %+v", rep.Problems)
	}
	if len(rep.GoPacks) != 1 || rep.GoPacks[0].Module != "github.com/acme/bff" {
		t.Fatalf("go pack not reported: %+v", rep.GoPacks)
	}
	var found bool
	for _, p := range rep.Problems {
		if p.Code == CodeGoPackNotDeployable {
			found = true
			if p.Fatal {
				t.Fatalf("the Go-pack problem is reported, not fatal: %+v", p)
			}
			if p.Scope != GoPackDir {
				t.Fatalf("the refusal is per-half and must name it: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("want a %s problem, got %+v", CodeGoPackNotDeployable, rep.Problems)
	}
	if len(rep.Deployables) != 2 {
		t.Fatalf("the other halves still analyze: %+v", rep.Deployables)
	}
}

// TestAKnownUnofferedKindIsReportedAndTheRestStillDeploys is the target
// model's second case (design section B, D9): `ios`, `android` and `macos` are
// written down as shapes and not registered, so a manifest declaring one gets
// a per-app problem that is NOT fatal to the package -- reported exactly as a
// Go pack is -- and the offered apps beside it analyze clean.
func TestAKnownUnofferedKindIsReportedAndTheRestStillDeploys(t *testing.T) {
	rep, err := Analyze(unofferedTargetPackage(), Options{})
	if err != nil {
		t.Fatalf("an unoffered target must not refuse the package: %v", err)
	}
	if !rep.OK {
		t.Fatalf("the rest of the package must still deploy: %+v", rep.Problems)
	}
	if len(rep.Deployables) != 2 {
		t.Fatalf("both declared apps must be in the report: %+v", rep.Deployables)
	}

	var docs, mobile *DeployableReport
	for i := range rep.Deployables {
		switch rep.Deployables[i].Name {
		case "docs":
			docs = &rep.Deployables[i]
		case "mobile":
			mobile = &rep.Deployables[i]
		}
	}
	if docs == nil || mobile == nil {
		t.Fatalf("report is missing an app: %+v", rep.Deployables)
	}
	if docs.Problem != nil {
		t.Fatalf("the offered app beside an unoffered one must analyze clean: %+v", docs.Problem)
	}
	if mobile.Problem == nil {
		t.Fatal("the iOS app must carry its problem on the row it is about")
	}
	if mobile.Problem.Code != CodeDeployableTargetNotOffered {
		t.Fatalf("want %s, got %+v", CodeDeployableTargetNotOffered, mobile.Problem)
	}
	if mobile.Problem.Fatal {
		t.Fatalf("not offered is scoped to the app and NOT fatal to the package: %+v", mobile.Problem)
	}
	if mobile.Problem.Scope != "mobile" {
		t.Fatalf("the problem must name the app it is about: %+v", mobile.Problem)
	}
	// The sentence is the product copy for this stop: it names the display
	// name, not the manifest value, and says "yet" because the kind is a
	// shape the model has written down.
	if !strings.Contains(mobile.Problem.Message, "iOS is not offered on this cluster yet") {
		t.Fatalf("want the not-offered sentence, got %q", mobile.Problem.Message)
	}
	// It is in the package-wide problem list too, non-fatal, so the What-it-is
	// stop and the confirm gate both see it.
	var listed bool
	for _, p := range rep.Problems {
		if p.Code == CodeDeployableTargetNotOffered {
			listed = true
			if p.Fatal {
				t.Fatalf("the listed problem must be non-fatal: %+v", p)
			}
		}
	}
	if !listed {
		t.Fatalf("want a %s problem in the report, got %+v", CodeDeployableTargetNotOffered, rep.Problems)
	}
}

// Every kind the target model knows and does not offer has a display name the
// sentence is built from, and none of them is an offered kind -- the two sets
// are disjoint by construction, or an app could be "not offered" and served.
func TestKnownUnofferedKindsAreNamedAndDisjointFromTheOfferedOnes(t *testing.T) {
	want := map[string]string{"ios": "iOS", "android": "Android", "macos": "macOS"}
	if len(KnownUnofferedKinds) != len(want) {
		t.Fatalf("want exactly %d known-unoffered kinds, got %v", len(want), KnownUnofferedKinds)
	}
	for kind, display := range want {
		target, ok := KnownUnofferedKinds[kind]
		if !ok {
			t.Errorf("kind %q is not in KnownUnofferedKinds", kind)
			continue
		}
		if target.Display != display {
			t.Errorf("kind %q: display %q, want %q", kind, target.Display, display)
		}
		if strings.TrimSpace(target.Address) == "" {
			t.Errorf("kind %q: the design's table names where it will go; the entry must too", kind)
		}
		if ValidKind(kind) {
			t.Errorf("kind %q is both offered and known-unoffered", kind)
		}
	}
}

func TestDslThatWouldRefuseBootIsAnAnalysisRefusal(t *testing.T) {
	// Control first: the same tree with a canonical relationship analyzes
	// clean, so the refusal below is about the relationship type and not about
	// the fixture being unparseable for some other reason.
	if _, err := Analyze(validPackage(), Options{}); err != nil {
		t.Fatalf("control failed: the valid fixture must analyze clean, got %v", err)
	}

	rep, err := Analyze(bootRefusingPackage(), Options{})
	if err == nil {
		t.Fatalf("DSL that refuses boot must refuse here: %+v", rep)
	}
	if got := RefusalCode(err); got != CodeDslRefusesBoot {
		t.Fatalf("want %s, got %s (%v)", CodeDslRefusesBoot, got, err)
	}
	// The construct-level errors strict boot would print have to travel, or
	// the refusal tells an author their DSL is broken and not what is broken.
	if !strings.Contains(err.Error(), "assignedTo") {
		t.Fatalf("the refusal must carry the construct-level error: %v", err)
	}
}

func TestReservedDslDomainRefusesRatherThanBeingSkipped(t *testing.T) {
	rep, err := Analyze(reservedDomainPackage(), Options{})
	if err == nil {
		t.Fatalf("a core-domain collision must refuse: %+v", rep)
	}
	if got := RefusalCode(err); got != CodeDslDomainReserved {
		t.Fatalf("want %s, got %s (%v)", CodeDslDomainReserved, got, err)
	}
	var reserved bool
	for _, d := range rep.DslDomains {
		if d.Domain == "cognition" && d.Reserved {
			reserved = true
		}
	}
	if !reserved {
		t.Fatalf("the reserved domain must be listed and marked: %+v", rep.DslDomains)
	}
}

// The analysis leaves the process exactly as it found it. This is the property
// that lets it run inside a serving node at all -- see package_gates.go.
func TestAnalysisLeavesTheProcessConceptRegistryUntouched(t *testing.T) {
	before := conceptRegistryCount()
	if before == 0 {
		t.Skip("no concepts registered in this binary; nothing to observe")
	}
	if conceptRegistryHas("v1:acme:widget") {
		t.Fatal("control failed: the candidate's concept is already registered before the pass")
	}

	if _, err := Analyze(validPackage(), Options{}); err != nil {
		t.Fatalf("analysis failed: %v", err)
	}

	if after := conceptRegistryCount(); after != before {
		t.Fatalf("the analysis changed the process registry: %d -> %d concepts", before, after)
	}
	if conceptRegistryHas("v1:acme:widget") {
		t.Fatal("a candidate package's concept leaked into the process registry")
	}
}
