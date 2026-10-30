package packages

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// analyze_access_gate_test.go -- a gate belongs to the package that ships it
// (design record 2026-10-29, D5 and the "Binding to the package" paragraph of
// section 7).
//
// A gate names a deployable and is bound, through the concept its construct
// reads or writes, to a DSL domain. At run time the bff opens a visitor's code
// only for a construct whose deployable is the site's own and whose domain its
// package ships, so a gate that breaks either rule could only ever answer 404.
// Analysis says so before anything is staged, and states the domains the
// package ships; a deploy records them (package.dslDomains) for the bff to read
// with the site, once its DSL is live (dsl_domains_deploy_test.go).
//
// These fixtures share process-wide pack registrations with the neighbouring
// analyze tests, so they do not call t.Parallel either.

const (
	// gateOnTheDocsApp names a deployable validPackage's manifest declares.
	gateOnTheDocsApp = `@accessGate(deployable="docs", gate="overview")`
	// gateOnAnotherProduct names one it does not: another product's, or a typo.
	gateOnAnotherProduct = `@accessGate(deployable="client-desk", gate="dashboard")`
)

// gatedPackage is validPackage whose acme domain also carries one gated query
// over its own concept. header is the annotation above it.
func gatedPackage(header string) fstest.MapFS {
	p := validPackage()
	p["dsl/acme/shapes.memql"] = file("@row\nshape widget acmeWidgetFull { }\n")
	p["dsl/acme/queries.memql"] = file(header + "\n@actor\nquery widget acmeWidgets {\n" +
		"  filter row => row.ownerUserId == actor.userId\n  paginate 20\n  shape acmeWidgetFull\n}\n")
	return p
}

// gatedCorePackage is the same package with the gated query bound to a concept
// the ENGINE owns, which this package does not ship and so cannot open a gate
// over.
func gatedCorePackage(header string) fstest.MapFS {
	p := validPackage()
	p["dsl/acme/shapes.memql"] = file("use platform.concepts.{ packState }\n\n@row\nshape packState acmePackStateFull { }\n")
	p["dsl/acme/queries.memql"] = file("use platform.concepts.{ packState }\n\n" + header + "\n@actor\nquery packState acmePackStates {\n" +
		"  filter row => row.enabled == true\n  paginate 20\n  shape acmePackStateFull\n}\n")
	return p
}

// problemsWithCode is every problem the report carries under one code.
func problemsWithCode(rep *Report, code string) []Problem {
	var out []Problem
	for _, p := range rep.Problems {
		if p.Code == code {
			out = append(out, p)
		}
	}
	return out
}

func TestAnalysisRefusesAGateNamingADeployableOutsideTheManifest(t *testing.T) {
	// The reachable positive: the same tree with the gate naming a deployable
	// this manifest declares analyzes clean, so the refusal below is about the
	// name and not about the fixture.
	if rep, err := Analyze(gatedPackage(gateOnTheDocsApp), Options{}); err != nil || !rep.OK || len(rep.Problems) != 0 {
		t.Fatalf("control failed: the gated fixture must analyze clean, got %v (problems %+v)", err, rep.Problems)
	}

	rep, err := Analyze(gatedPackage(gateOnAnotherProduct), Options{})
	if got := RefusalCode(err); got != CodeDslAccessGateForeignDeployable {
		t.Fatalf("want %s, got %q (%v) with problems %+v", CodeDslAccessGateForeignDeployable, got, err, rep.Problems)
	}
	if rep.OK {
		t.Fatalf("a refused package must not report OK: %+v", rep.Problems)
	}

	found := problemsWithCode(rep, CodeDslAccessGateForeignDeployable)
	if len(found) != 1 {
		t.Fatalf("want exactly one %s problem, got %+v", CodeDslAccessGateForeignDeployable, rep.Problems)
	}
	p := found[0]
	if !p.Fatal {
		t.Errorf("a gate that could only ever answer 404 is fatal, not a note: %+v", p)
	}
	// Scoped to the CONSTRUCT'S FILE, as the package author's checkout spells
	// it, so the repair is one jump away.
	if p.Scope != "dsl/acme/queries.memql" {
		t.Errorf("Scope = %q, want the construct's file", p.Scope)
	}
	if refusal, ok := err.(*Refusal); !ok || refusal.Scope != p.Scope {
		t.Errorf("the error Analyze returns is not this problem: %#v", err)
	}
	// The sentence names what to change: the construct, the name it used and
	// the names it could have used.
	for _, want := range []string{"acmeWidgets", "client-desk", `"docs"`, `"storefront"`} {
		if !strings.Contains(p.Message, want) {
			t.Errorf("the refusal does not say %q: %s", want, p.Message)
		}
	}
	// It is the only thing wrong with this tree, and nothing the engine's own
	// boot gates would say: the construct loads, it is simply not this
	// package's to open.
	for _, other := range rep.Problems {
		if other.Code != CodeDslAccessGateForeignDeployable {
			t.Errorf("unexpected second problem: %+v", other)
		}
	}

	t.Run("a package that declares no deployable at all", func(t *testing.T) {
		tree := gatedPackage(gateOnTheDocsApp)
		tree[ManifestName] = file("formatVersion: 1\nname: acme\n")
		rep, err := Analyze(tree, Options{})
		if got := RefusalCode(err); got != CodeDslAccessGateForeignDeployable {
			t.Fatalf("want %s, got %q (%v)", CodeDslAccessGateForeignDeployable, got, err)
		}
		p := problemsWithCode(rep, CodeDslAccessGateForeignDeployable)[0]
		if !strings.Contains(p.Message, "declares no deployable") {
			t.Errorf("the refusal does not say there is none to name: %s", p.Message)
		}
	})
}

func TestAnalysisAcceptsAGateNamingItsOwnDeployable(t *testing.T) {
	// Either of the manifest's two deployables, whatever their kind.
	for _, deployable := range []string{"docs", "storefront"} {
		t.Run(deployable, func(t *testing.T) {
			rep, err := Analyze(gatedPackage(`@accessGate(deployable="`+deployable+`", gate="overview")`), Options{})
			if err != nil || !rep.OK || len(rep.Problems) != 0 {
				t.Fatalf("a gate naming %q was refused: %v (problems %+v)", deployable, err, rep.Problems)
			}
		})
	}

	// A domain that pins its namespace is still the package's own. The gate
	// records the pin -- the namespace in its concept's id -- and so must the
	// list it is compared with, or this package could not gate anything.
	t.Run("in a domain whose namespace is pinned", func(t *testing.T) {
		tree := gatedPackage(gateOnTheDocsApp)
		tree["dsl/acme/namespace.pin"] = file("realns\n")
		rep, err := Analyze(tree, Options{})
		if err != nil || !rep.OK || len(rep.Problems) != 0 {
			t.Fatalf("a gate in a pinned domain was refused: %v (problems %+v)", err, rep.Problems)
		}
	})

	// Analysis reads a candidate and publishes nothing: this runs inside a
	// serving node, whose registry it must neither add to nor take from.
	if memql.AccessGateFor("docs", "overview", "acmeWidgets", []string{"acme"}) != nil {
		t.Error("analysing a candidate opened its gate on the live node")
	}
	for _, entry := range memql.AccessGateEntries() {
		if entry.Construct == "acmeWidgets" {
			t.Errorf("analysing a candidate put %+v in the live registry", entry)
		}
	}
}

// A cluster runs more than one product, and the validation runs over a view
// that holds all of them. A package is bound to the gates written in its OWN
// domains: another product's gate names that product's deployable and sits in
// that product's domain, and judging it against this manifest would refuse
// every deploy on a cluster that has one.
func TestAnalysisIgnoresTheGatesOfOtherProductsInstalledOnTheNode(t *testing.T) {
	memqldsl.RegisterTree("otherproduct", fstest.MapFS{
		"concepts.memql": file(validConcepts),
		"shapes.memql":   file("@row\nshape widget otherproductWidgetFull { }\n"),
		"queries.memql": file(`@accessGate(deployable="their-app", gate="desk")` + "\n@actor\nquery widget otherProductWidgets {\n" +
			"  filter row => row.ownerUserId == actor.userId\n  paginate 20\n  shape otherproductWidgetFull\n}\n"),
		"memql.toml": file(languageLine),
	})
	t.Cleanup(func() { memqldsl.UnregisterTree("otherproduct") })

	rep, err := Analyze(gatedPackage(gateOnTheDocsApp), Options{})
	if err != nil || !rep.OK || len(rep.Problems) != 0 {
		t.Fatalf("a clean package was refused beside another product's gate: %v (problems %+v)", err, rep.Problems)
	}
	// ...and the package's own gate is still judged: the control that makes the
	// pass above mean something.
	if _, err := Analyze(gatedPackage(gateOnAnotherProduct), Options{}); RefusalCode(err) != CodeDslAccessGateForeignDeployable {
		t.Fatalf("the package's own foreign gate was not refused beside another product: %v", err)
	}
}

func TestAnalysisRefusesAGateBoundToADomainThePackageDoesNotShip(t *testing.T) {
	// The control is TestAnalysisAcceptsAGateNamingItsOwnDeployable: the same
	// deployable, on a query bound to a concept of the package's own domain, is
	// clean.
	rep, err := Analyze(gatedCorePackage(gateOnTheDocsApp), Options{})
	if got := RefusalCode(err); got != CodeDslAccessGateForeignDomain {
		t.Fatalf("want %s, got %q (%v) with problems %+v", CodeDslAccessGateForeignDomain, got, err, rep.Problems)
	}
	if rep.OK {
		t.Fatalf("a refused package must not report OK: %+v", rep.Problems)
	}
	found := problemsWithCode(rep, CodeDslAccessGateForeignDomain)
	if len(found) != 1 {
		t.Fatalf("want exactly one %s problem, got %+v", CodeDslAccessGateForeignDomain, rep.Problems)
	}
	p := found[0]
	if !p.Fatal {
		t.Errorf("a gate that could only ever answer 404 is fatal, not a note: %+v", p)
	}
	if p.Scope != "dsl/acme/queries.memql" {
		t.Errorf("Scope = %q, want the construct's file", p.Scope)
	}
	// It says which domain the construct reaches into, which domains the
	// package does ship, and that the answer would always be a 404.
	for _, want := range []string{"acmePackStates", `"platform"`, `"acme"`, "404"} {
		if !strings.Contains(p.Message, want) {
			t.Errorf("the refusal does not say %q: %s", want, p.Message)
		}
	}
	// The deployable is this package's own, so that rule has nothing to say.
	if wrong := problemsWithCode(rep, CodeDslAccessGateForeignDeployable); len(wrong) != 0 {
		t.Errorf("the deployable is declared, yet it was refused as foreign: %+v", wrong)
	}

	// Both rules at once are both reported: an author fixing one should not
	// have to deploy again to learn of the other.
	both, err := Analyze(gatedCorePackage(gateOnAnotherProduct), Options{})
	if err == nil {
		t.Fatal("a gate wrong twice over was accepted")
	}
	if len(problemsWithCode(both, CodeDslAccessGateForeignDeployable)) != 1 || len(problemsWithCode(both, CodeDslAccessGateForeignDomain)) != 1 {
		t.Fatalf("want one problem for each rule, got %+v", both.Problems)
	}
}

// ---------------------------------------------------------------------------
// package.dslDomains: what the analysis puts on its report
// ---------------------------------------------------------------------------

// shippedBy is the list a package would record, as the analysis of its tree
// states it: dslDomainsFrom of the report. Recording it is the DEPLOY's act
// (dsl_domains_deploy_test.go); analysis only says what the tree ships.
func shippedBy(t *testing.T, tree fstest.MapFS) []string {
	t.Helper()
	rep, err := Analyze(tree, Options{})
	if err != nil {
		t.Fatalf("setup: the tree was refused: %v (problems %+v)", err, rep.Problems)
	}
	return dslDomainsFrom(rep)
}

func sameList(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestAnalysisRecordsTheDomainsThePackageShips(t *testing.T) {
	withDomain := func(tree fstest.MapFS, domain string) fstest.MapFS {
		tree["dsl/"+domain+"/concepts.memql"] = file(validConcepts)
		tree["dsl/"+domain+"/memql.toml"] = file(languageLine)
		return tree
	}

	t.Run("every domain it ships, sorted", func(t *testing.T) {
		if got := shippedBy(t, withDomain(validPackage(), "beta")); !sameList(got, []string{"acme", "beta"}) {
			t.Fatalf("the analysis ships %v, want [acme beta]", got)
		}
	})

	// THE POINT OF SPELLING IT AS A CONCEPT ID DOES. A gate records the domain
	// of the concept it is bound to, and the concept's id carries the pin.
	t.Run("a namespace.pin records the pin and not the directory", func(t *testing.T) {
		tree := validPackage()
		tree["dsl/pinneddir/concepts.memql"] = file(validConcepts)
		tree["dsl/pinneddir/memql.toml"] = file(languageLine)
		tree["dsl/pinneddir/namespace.pin"] = file("realns\n")
		if got := shippedBy(t, tree); !sameList(got, []string{"acme", "realns"}) {
			t.Fatalf("the analysis ships %v, want [acme realns]: a pinned domain is spelled by its pin", got)
		}
	})

	// A NAME WITH NO CONCEPT BEHIND IT IS NOT SHIPPED (the list is built from
	// the concepts the load produced): a domain that declares none contributes
	// nothing, and a pin on a directory that declares none claims nothing.
	t.Run("a domain declaring no concept contributes nothing", func(t *testing.T) {
		tree := validPackage()
		tree["dsl/quiet/notes.memql"] = file("// nothing here yet\n")
		tree["dsl/quiet/memql.toml"] = file(languageLine)
		if got := shippedBy(t, tree); !sameList(got, []string{"acme"}) {
			t.Fatalf("the analysis ships %v, want only [acme]", got)
		}
	})

	t.Run("a pin with no concept behind it claims nothing", func(t *testing.T) {
		tree := validPackage()
		tree["dsl/claiming/notes.memql"] = file("// constructs would go here\n")
		tree["dsl/claiming/memql.toml"] = file(languageLine)
		tree["dsl/claiming/namespace.pin"] = file("claimed\n")
		if got := shippedBy(t, tree); !sameList(got, []string{"acme"}) {
			t.Fatalf("the analysis ships %v, want only [acme]: a pin file alone made the package the owner of a namespace", got)
		}
	})

	// A name the engine owns is never shipped, whether it arrives as a
	// directory (refused outright) or as a pin (which the engine accepts).
	t.Run("an engine domain is never shipped", func(t *testing.T) {
		rep, err := Analyze(reservedDomainPackage(), Options{})
		if RefusalCode(err) != CodeDslDomainReserved {
			t.Fatalf("setup: want %s, got %v", CodeDslDomainReserved, err)
		}
		if got := dslDomainsFrom(rep); !sameList(got, []string{"acme"}) {
			t.Fatalf("a refused tree lists %v, want only [acme]: the reserved directory is not shipped", got)
		}
		tree := validPackage()
		tree["dsl/pinnedtocore/concepts.memql"] = file(validConcepts)
		tree["dsl/pinnedtocore/memql.toml"] = file(languageLine)
		tree["dsl/pinnedtocore/namespace.pin"] = file("library\n")
		if got := shippedBy(t, tree); !sameList(got, []string{"acme"}) {
			t.Fatalf("the analysis ships %v, want only [acme]: a pin naming an engine domain was listed", got)
		}
	})

	// A package with no DSL has an empty list, never nil: the concept's
	// []string refuses the word null.
	t.Run("a package with no DSL ships an empty list", func(t *testing.T) {
		got := shippedBy(t, spaOnlyPackage())
		if got == nil || len(got) != 0 {
			t.Fatalf("the analysis ships %#v, want an empty list", got)
		}
	})

	t.Run("the report names the namespaces", func(t *testing.T) {
		tree := validPackage()
		tree["dsl/pinneddir/concepts.memql"] = file(validConcepts)
		tree["dsl/pinneddir/memql.toml"] = file(languageLine)
		tree["dsl/pinneddir/namespace.pin"] = file("realns\n")
		rep, err := Analyze(tree, Options{})
		if err != nil {
			t.Fatal(err)
		}
		byDomain := map[string][]string{}
		for _, d := range rep.DslDomains {
			byDomain[d.Domain] = d.Namespaces
		}
		if len(byDomain["acme"]) != 1 || byDomain["acme"][0] != "acme" ||
			len(byDomain["pinneddir"]) != 1 || byDomain["pinneddir"][0] != "realns" {
			t.Fatalf("the report's domains carry %v, want acme -> acme and pinneddir -> realns", byDomain)
		}
	})
}

func TestDslDomainsFromListsEachShippedNamespaceOnceAndNeverAReservedOne(t *testing.T) {
	got := dslDomainsFrom(&Report{DslDomains: []DslDomainReport{
		{Domain: "beta", Namespaces: []string{"betans"}},
		{Domain: "acme", Namespaces: []string{"acme", "acmebilling"}},
		// Another directory pinned to a namespace the package already ships.
		{Domain: "alias", Namespaces: []string{"acme"}},
		// A domain the engine owns is refused, and what it would have named is
		// not the package's to open anything in.
		{Domain: "library", Reserved: true, Namespaces: []string{"library"}},
		{Domain: "blank", Namespaces: []string{"", "  "}},
	}})
	want := []string{"acme", "acmebilling", "betans"}
	if len(got) != len(want) {
		t.Fatalf("dslDomainsFrom = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dslDomainsFrom = %v, want %v", got, want)
		}
	}
}

func TestDslDomainsFromIsAnArrayNeverNull(t *testing.T) {
	// The trap declaredFrom documents: jsonLiteral sees an interface holding a
	// typed nil slice as non-nil, marshals it and writes the word null, which
	// the concept's []string refuses.
	for _, rep := range []*Report{nil, {}, {DslDomains: nil}, {DslDomains: []DslDomainReport{{Domain: "library", Reserved: true}}}} {
		got := dslDomainsFrom(rep)
		if got == nil {
			t.Fatalf("dslDomainsFrom(%+v) returned nil; the store would render null", rep)
		}
		if lit := jsonLiteral(got); lit != "[]" {
			t.Errorf("dslDomainsFrom(%+v) rendered %s, want []", rep, lit)
		}
	}
}
