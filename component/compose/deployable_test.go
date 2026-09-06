package compose

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

func fixturePackage() PackageSource {
	return PackageSource{
		Name: "Acme storefront",
		Deployables: []Deployable{
			{
				Name: "web",
				Kind: DeployableSPA,
				Files: []DeployableFile{
					{Path: "index.html", Body: []byte("<!doctype html><title>Acme</title>")},
					{Path: "assets/app.js", Body: []byte("console.log('hi')")},
				},
			},
		},
	}
}

func readZip(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("not a readable zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", f.Name, err)
		}
		out[f.Name] = string(body)
	}
	return out
}

// TestPackageManifestSitsAtTheRoot pins the one placement the pipeline
// refuses: `component/packages` reads memql-package.yaml at the ROOT of
// the zip, and a package whose manifest is one directory down is refused
// package_manifest_missing -- the exact failure source.go's tarball
// comment records having hit once already.
func TestPackageManifestSitsAtTheRoot(t *testing.T) {
	res, err := BuildPackageSource(fixturePackage(), fixtureProvenance())
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	parts := readZip(t, res.Bytes)
	if _, ok := parts[manifestName]; !ok {
		names := make([]string, 0, len(parts))
		for n := range parts {
			names = append(names, n)
		}
		t.Fatalf("%s is not at the zip root; entries were %v", manifestName, names)
	}
	for name := range parts {
		if strings.HasSuffix(name, "/"+manifestName) {
			t.Fatalf("a second manifest at %s -- the pipeline reads the root one and a reader cannot tell which is authoritative", name)
		}
	}
}

func TestPackageManifestDeclaresEachAppAtTheDirectoryItsFilesAreIn(t *testing.T) {
	res, err := BuildPackageSource(fixturePackage(), fixtureProvenance())
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	parts := readZip(t, res.Bytes)
	manifest := parts[manifestName]

	// The manifest's `path` and the zip entries must agree. They are
	// both derived from the app name for this reason -- a package whose
	// manifest points at a directory the zip does not contain fails at
	// build time with a message about a missing path rather than about
	// the mistake.
	if !strings.Contains(manifest, `path: "apps/web"`) {
		t.Fatalf("manifest does not declare the app's directory:\n%s", manifest)
	}
	for _, want := range []string{"apps/web/index.html", "apps/web/assets/app.js"} {
		if _, ok := parts[want]; !ok {
			t.Fatalf("%s is missing from the package; the manifest points at a directory the zip does not hold", want)
		}
	}
	if !strings.Contains(manifest, `kind: "spa"`) {
		t.Fatalf("manifest does not declare the kind:\n%s", manifest)
	}
	if !strings.Contains(manifest, "formatVersion: 1") {
		t.Fatalf("manifest declares no formatVersion:\n%s", manifest)
	}
}

// TestPackageSourceIsByteIdenticalAcrossRuns is the replay claim as a
// property: two runs of one recipe over unchanged sources must produce
// the same bytes. A zip stamped with time.Now() differs every run, and
// Deployables' content addressing keys on the digest -- so a moving one
// re-stages bytes that had not changed and reports a deploy of nothing.
func TestPackageSourceIsByteIdenticalAcrossRuns(t *testing.T) {
	first, err := BuildPackageSource(fixturePackage(), fixtureProvenance())
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	second, err := BuildPackageSource(fixturePackage(), fixtureProvenance())
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	if first.SHA256() != second.SHA256() {
		t.Fatalf("two runs produced different bytes: %s vs %s", first.SHA256(), second.SHA256())
	}

	// THE REACHABLE POSITIVE: a package that genuinely differs must
	// produce a different digest, or the assertion above would pass
	// against a builder that returned a constant.
	changed := fixturePackage()
	changed.Deployables[0].Files[0].Body = []byte("<!doctype html><title>Different</title>")
	third, err := BuildPackageSource(changed, fixtureProvenance())
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	if third.SHA256() == first.SHA256() {
		t.Fatal("a changed file produced the same digest -- the determinism assertion above proves nothing")
	}
}

// TestPackageSourceRefusesPathsThatEscape is the zip-slip guard, written
// as a refusal rather than a sanitisation: silently rewriting
// "../../etc/passwd" to "etc/passwd" produces a package that deploys
// something nobody asked for.
func TestPackageSourceRefusesPathsThatEscape(t *testing.T) {
	for _, bad := range []string{"../escape.html", "/absolute.html", "a/../../b.html", "", "a\\b.html", "./"} {
		pkg := fixturePackage()
		pkg.Deployables[0].Files = []DeployableFile{{Path: bad, Body: []byte("x")}}
		if _, err := BuildPackageSource(pkg, fixtureProvenance()); err == nil {
			t.Fatalf("path %q was accepted; it must be refused with the file named", bad)
		}
	}
	// The control: an ordinary nested path is accepted, so the loop
	// above is about the paths rather than about the builder failing on
	// everything.
	pkg := fixturePackage()
	pkg.Deployables[0].Files = []DeployableFile{{Path: "deep/nested/ok.html", Body: []byte("x")}}
	if _, err := BuildPackageSource(pkg, fixtureProvenance()); err != nil {
		t.Fatalf("an ordinary nested path was refused: %v", err)
	}
}

// TestPackageSourceNormalisesARedundantSeparator pins the OTHER side of
// the line safeRelPath draws. "a//b.html" means the same file after
// cleaning as before it, so it is normalised rather than refused -- and
// the one hazard that carries, two spellings of one entry, is caught by
// the duplicate check, which compares cleaned paths.
func TestPackageSourceNormalisesARedundantSeparator(t *testing.T) {
	pkg := fixturePackage()
	pkg.Deployables[0].Files = []DeployableFile{{Path: "assets//app.js", Body: []byte("x")}}
	res, err := BuildPackageSource(pkg, fixtureProvenance())
	if err != nil {
		t.Fatalf("a redundant separator was refused: %v", err)
	}
	parts := readZip(t, res.Bytes)
	if _, ok := parts["apps/web/assets/app.js"]; !ok {
		t.Fatalf("the separator was not normalised; entries were %v", parts)
	}

	// And the two spellings must collide rather than producing a zip
	// with two entries a reader would disagree about.
	pkg.Deployables[0].Files = []DeployableFile{
		{Path: "assets//app.js", Body: []byte("one")},
		{Path: "assets/app.js", Body: []byte("two")},
	}
	if _, err := BuildPackageSource(pkg, fixtureProvenance()); err == nil {
		t.Fatal("two spellings of one entry were accepted; the duplicate check must compare cleaned paths")
	}
}

func TestPackageSourceRefusesADuplicateEntry(t *testing.T) {
	pkg := fixturePackage()
	pkg.Deployables[0].Files = []DeployableFile{
		{Path: "index.html", Body: []byte("one")},
		{Path: "index.html", Body: []byte("two")},
	}
	if _, err := BuildPackageSource(pkg, fixtureProvenance()); err == nil {
		t.Fatal("a duplicate entry was accepted; a zip whose contents depend on which entry the reader picks is one readers disagree about")
	}
}

func TestPackageSourceCarriesProvenanceInAFileAndNotInTheManifest(t *testing.T) {
	p := fixtureProvenance()
	res, err := BuildPackageSource(fixturePackage(), p)
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	if !res.Embedded {
		t.Fatal("a package source has somewhere to put provenance, so it must report Embedded")
	}
	parts := readZip(t, res.Bytes)
	doc, ok := parts["PROVENANCE.md"]
	if !ok {
		t.Fatal("no PROVENANCE.md at the package root")
	}
	for _, want := range []string{p.CompositionId, p.GoalId, p.AuthorName, p.Instance} {
		if !strings.Contains(doc, want) {
			t.Fatalf("PROVENANCE.md omits %q:\n%s", want, doc)
		}
	}
	// THE MANIFEST IS A CONTRACT ANOTHER COMPONENT PARSES. Adding
	// unrecognised keys to it is how a deploy starts failing on a field
	// nobody remembers adding, so the provenance stays out of it.
	manifest := parts[manifestName]
	for _, mustNot := range []string{p.CompositionId, p.GoalId, p.AuthorId} {
		if strings.Contains(manifest, mustNot) {
			t.Fatalf("the manifest carries %q; provenance belongs in PROVENANCE.md, not in the document the pipeline validates:\n%s", mustNot, manifest)
		}
	}
}

func TestParseDeployableKindNamesTheDistributionTargetsSeparately(t *testing.T) {
	for _, ok := range []string{"spa", "static", "shopify_storefront"} {
		if _, err := ParseDeployableKind(ok); err != nil {
			t.Fatalf("%s must parse: %v", ok, err)
		}
	}
	if _, err := ParseDeployableKind("nonsense"); err == nil {
		t.Fatal("an unknown kind must be refused rather than defaulted")
	}
	// ios / android / macos are deliberately not site kinds, and the
	// refusal says which it is -- a person who types one has a real
	// intention and "unknown kind" tells them nothing.
	for _, name := range []string{"ios", "android", "macos"} {
		_, err := ParseDeployableKind(name)
		if err == nil {
			t.Fatalf("%s must be refused", name)
		}
		if !strings.Contains(err.Error(), "distribution target") {
			t.Fatalf("%s refusal reads as an unknown kind rather than as a deliberate absence: %v", name, err)
		}
	}
}

// TestPackageManifestSurvivesAControlByteInAName is docx.go's escapeXML
// defect one format along, and it is the same class: a composition's name
// is free text somebody typed, YAML 1.2 forbids unescaped C0 controls in a
// double-quoted scalar, and this manifest is a document ANOTHER component
// parses. One stray 0x0B produced a package the Deployables pipeline
// refuses with a parse error naming nothing.
func TestPackageManifestSurvivesAControlByteInAName(t *testing.T) {
	pkg := fixturePackage()
	pkg.Name = "Acme\x0bstorefront"
	res, err := BuildPackageSource(pkg, fixtureProvenance())
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	manifest := readZip(t, res.Bytes)[manifestName]
	for _, b := range []byte(manifest) {
		if b < 0x20 && b != '\n' && b != '\t' {
			t.Fatalf("the manifest carries raw control byte %#x; the pipeline's YAML parser refuses it:\n%s", b, manifest)
		}
	}
	// And the surrounding name survives -- the byte is dropped, the words
	// are not.
	if !strings.Contains(manifest, "Acmestorefront") {
		t.Fatalf("dropping the control byte took the name with it:\n%s", manifest)
	}
}

// TestPackageManifestQuotingEscapesTheBackslashFirst pins the one line
// every function of this shape is right or wrong on. Escaping the QUOTE
// first turns `"` into `\"` and then the backslash pass turns that into
// `\\"` -- a closed scalar followed by junk.
func TestPackageManifestQuotingEscapesTheBackslashFirst(t *testing.T) {
	pkg := fixturePackage()
	pkg.Name = `a"b\c`
	res, err := BuildPackageSource(pkg, fixtureProvenance())
	if err != nil {
		t.Fatalf("BuildPackageSource: %v", err)
	}
	manifest := readZip(t, res.Bytes)[manifestName]
	if !strings.Contains(manifest, `name: "a\"b\\c"`) {
		t.Fatalf("quoting is wrong; a backslash-then-quote ordering produces this:\n%s", manifest)
	}
}

func TestPackageSourceRefusesAnEmptyPackage(t *testing.T) {
	if _, err := BuildPackageSource(PackageSource{Name: "x"}, fixtureProvenance()); err == nil {
		t.Fatal("a package with no deployables must be refused here rather than by the pipeline; the person is in the Materializer, not in Deployables")
	}
	if _, err := BuildPackageSource(PackageSource{Deployables: fixturePackage().Deployables}, fixtureProvenance()); err == nil {
		t.Fatal("a package with no name must be refused")
	}
}
