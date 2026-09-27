package memql

import (
	"strings"
	"testing"
	"testing/fstest"
)

// signature_concept_5433_test.go -- memql#5433: a construct signature whose
// concept does not resolve is refused at load, by name, with a code.
//
// THE DEFECT. The file-top import table records a name-only entry for an
// import that resolves to no concept (most imports name shapes, traits and
// functions, so that is not itself wrong), and the signature pass skipped
// every name already in the table. A signature concept an import named but
// did not supply therefore bound to that entry's EMPTY id: a query with no
// filter loaded and matched nothing, a query with a filter was refused as
// "`concept==\"\"` does not lower" -- naming neither the concept nor the
// import -- and a mutation loaded with an empty write target.

// sigFile is one fixture file under a domain, with the domain's language line
// supplied by lint.
func sigTree(files map[string]string) fstest.MapFS {
	root := fstest.MapFS{}
	for p, src := range files {
		root[p] = &fstest.MapFile{Data: []byte(src)}
	}
	return root
}

// sigRefusal is the one diagnostic whose message names wantConstruct, failing
// the test when there is none.
func sigRefusal(t *testing.T, diags []LintDiagnostic, wantConstruct string) LintDiagnostic {
	t.Helper()
	for _, d := range diags {
		if strings.Contains(d.Message, wantConstruct) {
			return d
		}
	}
	t.Fatalf("no diagnostic names %s -- the load accepted it. Diagnostics:\n%s", wantConstruct, sigDump(diags))
	return LintDiagnostic{}
}

func sigDump(diags []LintDiagnostic) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString("  " + d.File + ": " + d.Message + "\n")
	}
	return b.String()
}

// The import names a namespace that declares no such concept, and no domain
// declares one: the query with no filter used to LOAD and match nothing, the
// one with a filter was refused as `concept==""`, and the mutation loaded with
// an empty write target. All three are refused by name now.
func TestSignatureConceptImportedButDeclaredNowhereIsRefused(t *testing.T) {
	const imp = "use sigwhere5433.concepts.{ sigLead5433 }\n\n"
	diags := lint(t, sigTree(map[string]string{
		"sigimp5433/queries.memql": imp + `/// Every lead, newest first.
query sigLead5433 sigAllLeads5433 {
  sort "row.createdAt", "desc"
  paginate 20
}

/// The open leads.
query sigLead5433 sigOpenLeads5433 {
  filter row => row.status == "open"
  paginate 20
}
`,
		"sigimp5433/mutations.memql": imp + `/// Create a lead.
mutation sigLead5433 sigCreateLead5433 {
  args {
    title  string!
  }
  insert {
    title: args.title
  }
}
`,
	}))

	for _, construct := range []string{`"sigAllLeads5433"`, `"sigOpenLeads5433"`, `"sigCreateLead5433"`} {
		d := sigRefusal(t, diags, construct)
		if d.Code != SignatureConceptCode {
			t.Errorf("%s: code = %q, want %q -- the id is what a reworded message keeps\n  %s", construct, d.Code, SignatureConceptCode, d.Message)
		}
		for _, want := range []string{
			`signature concept "sigLead5433" does not resolve`,
			"`use sigwhere5433.concepts.{ sigLead5433 }` imports it",
			`declare it in sigwhere5433`,
		} {
			if !strings.Contains(d.Message, want) {
				t.Errorf("%s: the refusal does not say %q:\n  %s", construct, want, d.Message)
			}
		}
		if strings.Contains(d.Message, `concept==""`) {
			t.Errorf("%s: the refusal still reads as the empty binding it replaced:\n  %s", construct, d.Message)
		}
	}
}

// An import naming the wrong namespace, for a name two other domains declare:
// the refusal says which imports would choose, rather than that nothing does.
func TestSignatureConceptImportedFromTheWrongNamespaceNamesTheImports(t *testing.T) {
	concept := func(name string) string {
		return "/// A widget.\nconcept " + name + " {\n  title  string\n}\n"
	}
	diags := lint(t, sigTree(map[string]string{
		"sigtwina5433/concepts.memql": concept("sigWidget5433"),
		"sigtwinb5433/concepts.memql": concept("sigWidget5433"),
		"sigwrong5433/queries.memql": `use sigelsewhere5433.concepts.{ sigWidget5433 }

/// Every widget.
query sigWidget5433 sigAllWidgets5433 {
  sort "row.createdAt", "desc"
  paginate 20
}
`,
	}))
	d := sigRefusal(t, diags, `"sigAllWidgets5433"`)
	if d.Code != SignatureConceptCode {
		t.Errorf("code = %q, want %q", d.Code, SignatureConceptCode)
	}
	for _, want := range []string{
		"`use sigtwina5433.concepts.{ sigWidget5433 }`",
		"`use sigtwinb5433.concepts.{ sigWidget5433 }`",
	} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("the refusal does not name the import %s:\n  %s", want, d.Message)
		}
	}
}

// The unimported case refused before this change too; it carries the code now,
// and names the concrete import when a domain declares the concept.
func TestSignatureConceptNotImportedNamesTheImportToAdd(t *testing.T) {
	diags := lint(t, sigTree(map[string]string{
		"sigforeign5433/concepts.memql": "/// A gadget.\nconcept sigGadget5433 {\n  title  string\n}\n",
		"sigunimp5433/queries.memql": `/// Every gadget.
query sigGadget5433 sigAllGadgets5433 {
  sort "row.createdAt", "desc"
  paginate 20
}

/// Every gizmo, of a concept no domain declares.
query sigGizmo5433 sigAllGizmos5433 {
  sort "row.createdAt", "desc"
  paginate 20
}
`,
	}))
	gadgets := sigRefusal(t, diags, `"sigAllGadgets5433"`)
	if gadgets.Code != SignatureConceptCode || !strings.Contains(gadgets.Message, "`use sigforeign5433.concepts.{ sigGadget5433 }`") {
		t.Errorf("want the code and the import to add; got [%s] %s", gadgets.Code, gadgets.Message)
	}
	gizmos := sigRefusal(t, diags, `"sigAllGizmos5433"`)
	if gizmos.Code != SignatureConceptCode || !strings.Contains(gizmos.Message, `no mounted domain declares a concept "sigGizmo5433"`) {
		t.Errorf("want the code and that no domain declares it; got [%s] %s", gizmos.Code, gizmos.Message)
	}
}

// The positive control: an import that supplies the concept binds it, and the
// query loads. Without it the refusals above could be a loader that refuses
// every imported signature concept.
func TestSignatureConceptImportedAndDeclaredLoads(t *testing.T) {
	diags := lint(t, sigTree(map[string]string{
		"sigok5433/concepts.memql": "/// A lead.\nconcept sigOkLead5433 {\n  status  string\n}\n",
		"sigokuse5433/queries.memql": `use sigok5433.concepts.{ sigOkLead5433 }

/// The open leads.
query sigOkLead5433 sigOkOpenLeads5433 {
  filter row => row.status == "open"
  paginate 20
}
`,
	}))
	for _, d := range diags {
		if strings.Contains(d.Message, "sigOkOpenLeads5433") || strings.Contains(d.Message, "sigOkLead5433") {
			t.Errorf("an imported, declared signature concept was refused:\n  %s", d.Message)
		}
	}
}

// A seed writes its row through create<Concept>, resolved by bare name across
// every mounted domain. One whose concept no domain declares used to load and
// fail at materialization on every boot; it is refused at load. One binding a
// concept another domain declares, with no import, materializes -- the
// materializer never reads an import -- and still loads.
func TestSeedWhoseConceptNoDomainDeclaresIsRefused(t *testing.T) {
	diags := lint(t, sigTree(map[string]string{
		"sigseedhome5433/concepts.memql": `/// A banner.
concept sigBanner5433 {
  text  string
}
`,
		"sigseedhome5433/mutations.memql": `/// The mutation the seed materializer writes a banner through.
mutation sigBanner5433 createSigBanner5433 {
  args {
    sigBanner5433Id  string!
    text             string!
  }
  insert {
    id: args.sigBanner5433Id
    text: args.text
  }
}
`,
		"sigseed5433/seeds.memql": `/// A banner every cluster starts with, from a domain that does not declare it.
seed sigBanner5433 sigWelcomeBanner5433 {
  id: "welcome"
  text: "Welcome"
}

/// A seed of a concept no domain declares.
seed sigNowhere5433 sigGhostSeed5433 {
  id: "ghost"
  text: "Boo"
}
`,
	}))
	d := sigRefusal(t, diags, `"sigGhostSeed5433"`)
	if d.Code != SignatureConceptCode || !strings.Contains(d.Message, `no mounted domain declares a concept "sigNowhere5433"`) ||
		!strings.Contains(d.Message, "createSigNowhere5433") {
		t.Errorf("want the code, the concept and the mutation it would write through; got [%s] %s", d.Code, d.Message)
	}
	for _, other := range diags {
		if strings.Contains(other.Message, "sigWelcomeBanner5433") {
			t.Errorf("a seed of a concept another domain declares was refused -- the materializer resolves it:\n  %s", other.Message)
		}
	}
}

// A shape and a spec whose binding does not resolve were already refused;
// they carry the same code now, so one id names the defect on every construct
// with a signature.
func TestShapeAndSpecBindingRefusalsCarryTheCode(t *testing.T) {
	diags := lint(t, sigTree(map[string]string{
		"sigshape5433/shapes.memql": `/// A card of a concept no domain declares.
@row
shape sigNowhereCard5433 sigNowhereCardShape5433 {
  title
}
`,
		"sigspec5433/specs.memql": `/// Open, over a binding no domain declares.
spec sigNowhereSpec5433 isSigNowhereOpen5433 = row => row.status == "open"
`,
	}))
	for _, construct := range []string{`"sigNowhereCardShape5433"`, `"isSigNowhereOpen5433"`} {
		d := sigRefusal(t, diags, construct)
		if d.Code != SignatureConceptCode || !strings.HasSuffix(strings.TrimSpace(d.Message), "["+SignatureConceptCode+"]") {
			t.Errorf("%s: want the code, last in brackets; got [%s] %s", construct, d.Code, d.Message)
		}
	}
}

// The remedy names the right thing to do with the import that took the name.
// An import of a SHAPE that shares the concept's name is not the concept's
// import, and adding a concepts import beside it changes nothing (the first
// import naming a name is the one resolution reads): the fix aliases it. And
// the fix never suggests importing the file's own domain, whose concepts
// need no import. Each fix is followed below, and the result loads.
func TestSignatureConceptFixIsOneThatLoads(t *testing.T) {
	concept := func(name string) string { return "/// A record.\nconcept " + name + " {\n  title  string\n}\n" }
	query := func(imports, name string) string {
		return imports + "\n/// Every record.\nquery sigFixLead5433 " + name + " {\n  sort \"row.createdAt\", \"desc\"\n  paginate 20\n}\n"
	}
	base := map[string]string{
		"sigfixsales5433/concepts.memql": concept("sigFixLead5433"),
		"sigfixmkt5433/concepts.memql":   concept("sigFixLead5433"),
		"sigfixcrm5433/concepts.memql":   concept("sigFixCard5433"),
		// A shape of crm's own concept, named as the lead concept is.
		"sigfixcrm5433/shapes.memql": "/// A card.\n@row\nshape sigFixCard5433 sigFixLead5433 {\n  title\n}\n",
	}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	diags := lint(t, sigTree(with(map[string]string{
		"sigfixuse5433/queries.memql": query("use sigfixcrm5433.shapes.{ sigFixLead5433 }\n", "sigFixLeadsByShape5433"),
	})))
	d := sigRefusal(t, diags, `"sigFixLeadsByShape5433"`)
	for _, want := range []string{
		"the name is taken by `use sigfixcrm5433.shapes.{ sigFixLead5433 }`, which imports a shape, not a concept",
		"alias it -- `use sigfixcrm5433.shapes.{ sigFixLead5433 as <another name> }`",
		"`use sigfixmkt5433.concepts.{ sigFixLead5433 }`",
		"`use sigfixsales5433.concepts.{ sigFixLead5433 }`",
	} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("the shape-import refusal does not say %q:\n  %s", want, d.Message)
		}
	}
	if strings.Contains(d.Message, "`use sigfixcrm5433.shapes.{ sigFixLead5433 }` imports it") {
		t.Errorf("the refusal calls a shape import the concept's import:\n  %s", d.Message)
	}
	// Followed: the shape aliased, the concept imported.
	diags = lint(t, sigTree(with(map[string]string{
		"sigfixuse5433/queries.memql": query("use sigfixcrm5433.shapes.{ sigFixLead5433 as sigFixLeadCard5433 }\nuse sigfixsales5433.concepts.{ sigFixLead5433 }\n", "sigFixLeadsByShape5433"),
	})))
	for _, other := range diags {
		if strings.Contains(other.Message, "sigFixLeadsByShape5433") {
			t.Errorf("following the fix did not load:\n  %s", other.Message)
		}
	}

	// A concepts import naming neither of the two namespaces that declare
	// the concept -- one of them the file's own.
	own := map[string]string{
		"sigfixown5433/concepts.memql":   "/// A deal.\nconcept sigFixDeal5433 {\n  title  string\n}\n",
		"sigfixother5433/concepts.memql": "/// A deal.\nconcept sigFixDeal5433 {\n  title  string\n}\n",
	}
	ownQuery := func(imports string) string {
		return imports + "\n/// Every deal.\nquery sigFixDeal5433 sigFixDeals5433 {\n  sort \"row.createdAt\", \"desc\"\n  paginate 20\n}\n"
	}
	own["sigfixown5433/queries.memql"] = ownQuery("use sigfixnone5433.concepts.{ sigFixDeal5433 }\n")
	d = sigRefusal(t, lint(t, sigTree(own)), `"sigFixDeals5433"`)
	for _, want := range []string{
		"drop `use sigfixnone5433.concepts.{ sigFixDeal5433 }`: this file's own domain declares the concept",
		"in place of `use sigfixnone5433.concepts.{ sigFixDeal5433 }` from the namespace that declares it: `use sigfixother5433.concepts.{ sigFixDeal5433 }`",
	} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("the own-domain refusal does not say %q:\n  %s", want, d.Message)
		}
	}
	if strings.Contains(d.Message, "use sigfixown5433.concepts") {
		t.Errorf("the refusal suggests importing the file's own domain:\n  %s", d.Message)
	}
	// Followed: the import dropped.
	own["sigfixown5433/queries.memql"] = ownQuery("")
	for _, other := range lint(t, sigTree(own)) {
		if strings.Contains(other.Message, "sigFixDeals5433") {
			t.Errorf("following the fix did not load:\n  %s", other.Message)
		}
	}
}
