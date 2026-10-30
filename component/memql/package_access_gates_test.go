package memql

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// package_access_gates_test.go -- what a CANDIDATE package's analysis reports
// about its access gates and its domains, which is what lets component/packages
// bind a gate to the package that ships it (design record 2026-10-29, D5 and
// the "Binding to the package" paragraph of section 7).
//
// Two facts leave AnalyzePackageDSL for that. The gates the candidate loaded,
// because a package deploy validates with publish=false and so the live
// registry (AccessGateEntries) never holds them; and the domain each mounted
// directory goes by to an access gate, because the spelling a gate records is
// the namespace a concept id carries, which a namespace.pin moves off the
// directory name.

// gateCandidate is a candidate DSL root: one directory per domain, each with the
// concept the gated fixtures read, a shape of its own name (shape names are not
// shared between domains here) and the constructs given.
func gateCandidate(domains map[string]string) fstest.MapFS {
	root := fstest.MapFS{}
	for domain, constructs := range domains {
		root[domain+"/concepts.memql"] = &fstest.MapFile{Data: []byte(gateConceptSource)}
		root[domain+"/shapes.memql"] = &fstest.MapFile{Data: []byte("@row\nshape deskBrief " + domain + "BriefFull { }\n")}
		root[domain+"/queries.memql"] = &fstest.MapFile{Data: []byte(constructs)}
	}
	return withLanguageLines(root)
}

// gateCandidateQuery is a gated query over the fixture concept that declares no
// argument, so the only thing a case built from it can be about is the rule it
// names.
func gateCandidateQuery(header, name, domain string) string {
	return header + "\n@actor\nquery deskBrief " + name + " {\n" +
		"  filter row => row.ownerUserId == actor.userId\n  paginate 20\n  shape " + domain + "BriefFull\n}\n"
}

// A package deploy validates its candidate with publish=false, so the entries
// have to come out of the load itself: reading them back from the registry
// would answer with the live node's gates and not the candidate's.
func TestLoadAccessGatesReturnsTheViewItBuiltWhetherOrNotItPublishes(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()

	candidate, problems := loadGateBundleEntries(t, registryBundle(), false)
	if len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused:\n  %s", strings.Join(problems, "\n  "))
	}
	if len(candidate) == 0 {
		t.Fatal("a candidate load returned no entries, so a package deploy cannot see the gates it is about to ship")
	}
	if live := AccessGateEntries(); len(live) != 0 {
		t.Fatalf("a candidate load published into the node it runs on: %+v", live)
	}

	published, problems := loadGateBundleEntries(t, registryBundle(), true)
	if len(problems) != 0 {
		t.Fatalf("setup: the bundle was refused on the second load:\n  %s", strings.Join(problems, "\n  "))
	}
	if !reflect.DeepEqual(candidate, published) {
		t.Fatalf("the candidate view is not the view a boot publishes:\n candidate %+v\n published %+v", candidate, published)
	}
	if live := AccessGateEntries(); !reflect.DeepEqual(live, candidate) {
		t.Fatalf("the returned entries are not the registry's own, in its own order:\n live      %+v\n candidate %+v", live, candidate)
	}
}

// A refused construct is not a gate, and neither is a disabled one: the entries
// are the view, and the view is only what would open.
func TestLoadAccessGatesReturnsOnlyWhatWouldOpen(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()

	files := gateBundleDomains(map[string]string{
		"desk": gateQuery(gateDashboard, "deskBriefs", codeArgs, codeFilter) +
			gateQuery(gateOverview+"\n@serverOnly", "refusedOne", codeArgs, codeFilter) +
			gateQuery(gateOverview+"\n@disabled", "disabledOne", codeArgs, codeFilter),
	})
	entries, problems := loadGateBundleEntries(t, files, false)
	if len(problems) != 1 || !strings.Contains(problems[0], "refusedOne") {
		t.Fatalf("setup: want the one refusal, got %v", problems)
	}
	if len(entries) != 1 || entries[0].Construct != "deskBriefs" {
		t.Fatalf("the view holds %+v, want only deskBriefs: a refused construct and a disabled one open nothing", entries)
	}
}

func TestAnalyzePackageDSLReportsTheGatesTheCandidateDeclares(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()

	result, err := AnalyzePackageDSL(discardLogger(), gateCandidate(map[string]string{
		"desk": gateCandidateQuery(gateDashboard, "deskBriefs", "desk"),
		"shipping": gateCandidateQuery(`@accessGate(deployable="shipping-desk", gate="desk")`,
			"shippingBriefs", "shipping"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("setup: the candidate was refused: %+v", result.Diagnostics)
	}

	if len(result.AccessGates) != 2 {
		t.Fatalf("AccessGates = %+v, want the candidate's two gates", result.AccessGates)
	}
	// Sorted as AccessGateEntries is: by deployable, then gate, then construct.
	first, second := result.AccessGates[0], result.AccessGates[1]
	if first.Deployable != "client-desk" || second.Deployable != "shipping-desk" {
		t.Fatalf("the gates are not in the registry's order: %+v", result.AccessGates)
	}
	if first.Gate != "dashboard" || first.Construct != "deskBriefs" || first.Kind != AccessKindQuery ||
		first.Domain != "desk" || first.Namespace != "desk" {
		t.Errorf("first gate = %+v", first)
	}
	// The file an author is pointed at: the construct's, relative to the DSL
	// root, spelled as a LoadReport skip spells it.
	if first.File != "desk/queries.memql" || second.File != "shipping/queries.memql" {
		t.Errorf("File = %q and %q, want each construct's own file", first.File, second.File)
	}

	// The analysis read the candidate and published nothing: it runs inside a
	// serving node, whose own gates it must neither add to nor take from.
	if live := AccessGateEntries(); len(live) != 0 {
		t.Fatalf("analysing a candidate changed the live registry: %+v", live)
	}
}

// A gate belongs to the package that ships the concept its construct is bound
// to, so the entry has to say which concept that is -- not which file the
// construct happens to be written in.
func TestAnalyzePackageDSLRecordsTheDomainOfTheConceptAndTheFileOfTheConstruct(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()

	root := gateCandidate(map[string]string{
		"conceptdomain": "",
	})
	// A second domain that only holds a construct, bound to the first domain's
	// concept through an import.
	root["constructdomain/queries.memql"] = &fstest.MapFile{Data: []byte(
		"use conceptdomain.concepts.{ deskBrief }\nuse conceptdomain.shapes.{ conceptdomainBriefFull }\n\n" +
			gateCandidateQuery(gateDashboard, "importedBriefs", "conceptdomain"))}
	withLanguageLines(root)

	result, err := AnalyzePackageDSL(discardLogger(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("setup: the candidate was refused: %+v", result.Diagnostics)
	}
	if len(result.AccessGates) != 1 {
		t.Fatalf("AccessGates = %+v, want the one gate", result.AccessGates)
	}
	gate := result.AccessGates[0]
	if gate.Domain != "conceptdomain" {
		t.Errorf("Domain = %q, want the domain of the concept it is bound to", gate.Domain)
	}
	if gate.Namespace != "constructdomain" || gate.File != "constructdomain/queries.memql" {
		t.Errorf("Namespace = %q, File = %q, want the construct's own directory and file", gate.Namespace, gate.File)
	}
}

// THE VIEW COVERS THE WHOLE TREE THE VALIDATION RAN OVER: the engine's, every
// other product installed on this node, and the candidate. A package is bound
// to the gates ITS OWN domains declare. Reporting another product's would make
// every deploy on a cluster that runs two of them judge one against the other's
// manifest, and refuse a package for a gate it does not contain.
func TestAnalyzePackageDSLReportsOnlyTheCandidatesOwnGates(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()

	// Another product is installed here, with a gate of its own on a deployable
	// of its own.
	memqldsl.RegisterTree("installedproduct", withLanguageLine(fstest.MapFS{
		"concepts.memql": {Data: []byte(gateConceptSource)},
		"shapes.memql":   {Data: []byte("@row\nshape deskBrief installedproductBriefFull { }\n")},
		"queries.memql": {Data: []byte(gateCandidateQuery(`@accessGate(deployable="installed-app", gate="desk")`,
			"installedBriefs", "installedproduct"))},
	}))
	t.Cleanup(func() { memqldsl.UnregisterTree("installedproduct") })

	result, err := AnalyzePackageDSL(discardLogger(), gateCandidate(map[string]string{
		"desk": gateCandidateQuery(gateDashboard, "deskBriefs", "desk"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("setup: the candidate was refused beside an installed product: %+v", result.Diagnostics)
	}
	if len(result.AccessGates) != 1 || result.AccessGates[0].Construct != "deskBriefs" {
		t.Fatalf("AccessGates = %+v, want only the candidate's own gate: the installed product's is not this package's", result.AccessGates)
	}

	// A candidate that UPDATES an installed package's domain -- the ordinary
	// redeploy -- is read in its place, and what it replaced is not its gate.
	mountCandidateRuntimeDomain(t, "installedruntime", withLanguageLine(fstest.MapFS{
		"concepts.memql": {Data: []byte(gateConceptSource)},
		"shapes.memql":   {Data: []byte("@row\nshape deskBrief installedruntimeBriefFull { }\n")},
		"queries.memql": {Data: []byte(gateCandidateQuery(`@accessGate(deployable="installed-app", gate="desk")`,
			"oldBriefs", "installedruntime"))},
	}))
	t.Cleanup(func() { memqldsl.UnregisterTree("installedruntime") })

	updated, err := AnalyzePackageDSL(discardLogger(), gateCandidate(map[string]string{
		"installedruntime": gateCandidateQuery(`@accessGate(deployable="installed-app", gate="desk")`, "newBriefs", "installedruntime"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.AccessGates) != 1 || updated.AccessGates[0].Construct != "newBriefs" {
		t.Fatalf("AccessGates = %+v, want the update's gate and not the installed version's", updated.AccessGates)
	}
}

// What the candidate cannot open is not reported either: a refusal is a
// diagnostic and the construct is left out, so package analysis never treats a
// gate strict boot would have refused as a gate it must bind.
func TestAnalyzePackageDSLLeavesARefusedGateOutOfTheGatesItReports(t *testing.T) {
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()

	result, err := AnalyzePackageDSL(discardLogger(), gateCandidate(map[string]string{
		"desk": gateCandidateQuery(gateDashboard, "deskBriefs", "desk"),
		"partner":   gateCandidateQuery(gateOverview+"\n@serverOnly", "partnerBriefs", "partner"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	refused := false
	for _, d := range result.Diagnostics {
		refused = refused || (strings.Contains(d.Message, "partnerBriefs") && strings.Contains(d.Message, "@serverOnly"))
	}
	if !refused {
		t.Fatalf("setup: the contradiction was not reported: %+v", result.Diagnostics)
	}
	if len(result.AccessGates) != 1 || result.AccessGates[0].Construct != "deskBriefs" {
		t.Fatalf("AccessGates = %+v, want only the gate that would open", result.AccessGates)
	}
}

// ---------------------------------------------------------------------------
// THE DOMAINS A CANDIDATE SHIPS
// ---------------------------------------------------------------------------

// namespacesOf runs the analysis over a candidate root and answers the
// Namespaces it reports, after checking the candidate was not refused for any
// other reason.
func namespacesOf(t *testing.T, root fstest.MapFS) map[string][]string {
	t.Helper()
	restoreAccessGatesAfter(t)
	SetAccessGatesForTest()
	result, err := AnalyzePackageDSL(discardLogger(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("setup: the candidate was refused: %+v", result.Diagnostics)
	}
	return result.Namespaces
}

func TestAnalyzePackageDSLNamesEachMountedDomainByTheNamespaceItsConceptsCarry(t *testing.T) {
	t.Run("a directory that pins nothing goes by its own name", func(t *testing.T) {
		got := namespacesOf(t, gateCandidate(map[string]string{"plaindir": ""}))
		if want := []string{"plaindir"}; !reflect.DeepEqual(got["plaindir"], want) {
			t.Fatalf("Namespaces = %v, want %v", got, want)
		}
	})

	// THE POINT OF THE FIELD. The concept id is v1:<namespace>:name and a pin
	// moves the namespace off the directory, so a gate bound to one of these
	// concepts records the pin. A list spelled from directory names would never
	// match it, and the gate would be closed without a word.
	t.Run("a pinned directory goes by its pin and not by its name", func(t *testing.T) {
		root := gateCandidate(map[string]string{"pinneddir": ""})
		root["pinneddir/namespace.pin"] = &fstest.MapFile{Data: []byte("realns\n")}
		got := namespacesOf(t, root)
		if want := []string{"realns"}; !reflect.DeepEqual(got["pinneddir"], want) {
			t.Fatalf("Namespaces = %v, want %v: a pinned domain is not spelled by its directory", got, want)
		}
	})

	// A nested directory that holds concepts has to pin, because its namespace
	// is a path and a path is no namespace. What it pins is what its concepts
	// carry, which is not necessarily the domain's own name.
	t.Run("a nested directory that pins another namespace adds it", func(t *testing.T) {
		root := gateCandidate(map[string]string{"outerdir": ""})
		root["outerdir/inner/concepts.memql"] = &fstest.MapFile{Data: []byte(strings.Replace(gateConceptSource, "deskBrief", "innerBrief", 1))}
		root["outerdir/inner/namespace.pin"] = &fstest.MapFile{Data: []byte("innerns\n")}
		got := namespacesOf(t, root)
		if want := []string{"innerns", "outerdir"}; !reflect.DeepEqual(got["outerdir"], want) {
			t.Fatalf("Namespaces = %v, want %v", got, want)
		}
	})

	t.Run("a nested directory that pins nothing is a path and names nothing", func(t *testing.T) {
		root := gateCandidate(map[string]string{"pathdir": ""})
		root["pathdir/queries/more.memql"] = &fstest.MapFile{Data: []byte(
			"use pathdir.concepts.{ deskBrief }\nuse pathdir.shapes.{ pathdirBriefFull }\n\n" +
				gateCandidateQuery("", "moreBriefs", "pathdir"))}
		got := namespacesOf(t, root)
		if want := []string{"pathdir"}; !reflect.DeepEqual(got["pathdir"], want) {
			t.Fatalf("Namespaces = %v, want %v: \"pathdir/queries\" is not a namespace", got, want)
		}
	})

	// "If a domain has no constructs it still counts as shipped": a package that
	// ships only the name of a domain still owns it.
	t.Run("a domain holding no construct is still shipped", func(t *testing.T) {
		root := fstest.MapFS{"emptydir/notes.memql": &fstest.MapFile{Data: []byte("// nothing here yet\n")}}
		got := namespacesOf(t, withLanguageLines(root))
		if want := []string{"emptydir"}; !reflect.DeepEqual(got["emptydir"], want) {
			t.Fatalf("Namespaces = %v, want %v", got, want)
		}
	})

	// A name the engine owns is not a package's to ship however it arrives. A
	// directory carrying one is refused outright; a pin naming one is the same
	// name by another road, and the list it would put the package in must not
	// hold it.
	t.Run("a pin naming a domain the engine owns is not shipped", func(t *testing.T) {
		root := gateCandidate(map[string]string{"pinnedtocore": ""})
		root["pinnedtocore/namespace.pin"] = &fstest.MapFile{Data: []byte("library\n")}
		got := namespacesOf(t, root)
		if names, mounted := got["pinnedtocore"]; !mounted || len(names) != 0 {
			t.Fatalf("Namespaces = %v, want the domain mounted and shipping no engine namespace", got)
		}
	})

	// A domain the engine already owns is not mounted, so it is not shipped.
	t.Run("a domain the engine owns is not named", func(t *testing.T) {
		restoreAccessGatesAfter(t)
		SetAccessGatesForTest()
		root := gateCandidate(map[string]string{"plaindir": ""})
		root["library/concepts.memql"] = &fstest.MapFile{Data: []byte(gateConceptSource)}
		withLanguageLines(root)
		result, err := AnalyzePackageDSL(discardLogger(), root)
		if err != nil {
			t.Fatal(err)
		}
		if _, named := result.Namespaces["library"]; named {
			t.Fatalf("Namespaces names the reserved domain: %v", result.Namespaces)
		}
		if !reflect.DeepEqual(result.SkippedCore, []string{"library"}) {
			t.Fatalf("setup: SkippedCore = %v", result.SkippedCore)
		}
		if _, named := result.Namespaces["plaindir"]; !named {
			t.Fatalf("Namespaces dropped the domain beside it: %v", result.Namespaces)
		}
	})
}

// The two spellings are read by two readers -- the registry entry by the
// handler, the list by the same handler a moment later -- and they must be the
// same string, or a gate in a pinned domain is closed and nothing says so.
func TestAGateInAPinnedDomainRecordsASpellingTheDomainListHolds(t *testing.T) {
	for _, tc := range []struct {
		name, pin, domain string
	}{
		{"no pin", "", "desk"},
		{"a pin", "realns\n", "realns"},
		// A colon-scoped namespace is v1:scoped:sub:name, and the segment an
		// entry records is the first.
		{"a colon-scoped pin", "scoped:sub\n", "scoped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreAccessGatesAfter(t)
			SetAccessGatesForTest()
			root := gateCandidate(map[string]string{
				"desk": gateCandidateQuery(gateDashboard, "deskBriefs", "desk"),
			})
			if tc.pin != "" {
				root["desk/namespace.pin"] = &fstest.MapFile{Data: []byte(tc.pin)}
			}
			result, err := AnalyzePackageDSL(discardLogger(), root)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Diagnostics) != 0 {
				t.Fatalf("setup: the engine refused this namespace, so there is no gate to agree with: %+v", result.Diagnostics)
			}
			if len(result.AccessGates) != 1 || result.AccessGates[0].Domain != tc.domain {
				t.Fatalf("AccessGates = %+v, want one gate whose Domain is %q", result.AccessGates, tc.domain)
			}
			found := false
			for _, name := range result.Namespaces["desk"] {
				found = found || name == result.AccessGates[0].Domain
			}
			if !found {
				t.Fatalf("the gate records the domain %q and the domains list holds %v: the gate would never open",
					result.AccessGates[0].Domain, result.Namespaces["desk"])
			}
		})
	}
}
