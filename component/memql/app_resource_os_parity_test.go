package memql

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// THE OS REGISTRY AND THE APP SEEDS ARE PINNED TO EACH OTHER (epic
// memql#5288, task memql#5302; epic memql#5289, task memql#5304; app access
// grants design, sections 4 and 5).
//
// The MemQL OS registry (clients/os/src/apps/registry.tsx) names every app,
// floored section and widget as a capability RESOURCE -- `requires:
// "app:<id>"` on the manifest, `requires: "app:<id>/<section>"` on a
// section -- and the shell decides what to draw by asking whether the
// signed-in person's effective capability set holds `read` on that name.
// dsl/rbac/seeds.memql is where the names come from: a resource EXISTS by
// being seeded on at least one role, and the seeds were written to
// reproduce the registry's hand-written `roles:` floors exactly, so the
// switch from floors to names changed nobody's desktop.
//
// This gate reads the registry SOURCE -- the way
// TestSiteKindEnumMatchesOsOfferedKinds reads the offered kinds -- and fails
// the build when the two sides disagree in EITHER direction:
//
//   - a `requires:` naming a resource no seed declares is a surface NOBODY
//     can open: the shell hides what the effective set does not hold, and
//     no role holds a name the catalog never seeded;
//   - a seeded `read app:*` the registry does not ask for is a resource that
//     gates nothing and will drift;
//   - a manifest or section still carrying `roles:` is the retired form, and
//     the shell no longer reads it -- a floor written there gates nothing.
//
// The seeds are read through the REAL loader (LoadUnifiedSeeds) and lowered
// through the REAL catalog builder, so what is compared is the catalog a
// booted node installs, not a regex over the file.

// osRegistryPath is the MemQL OS app registry, relative to this package.
const osRegistryPath = "../../clients/os/src/apps/registry.tsx"

// osRegistryResources reads every app and widget manifest the registry lists
// and answers the set of capability resources it names: the manifest's own
// `requires:` and every section's.
//
// A section with NO `requires:` of its own is not a resource: it is reached
// through its app's door, and seeding it would be a second row saying what
// the app row already says. A MANIFEST with no `requires:` is a defect --
// every app is a resource, because opening it is a question the effective
// set has to answer -- and the parser refuses it below.
func osRegistryResources(t *testing.T) map[string]bool {
	t.Helper()
	reg := newOsRegistrySource(t, osRegistryPath)
	out := map[string]bool{}
	for _, m := range reg.manifests(t) {
		if m.requires == "" {
			t.Errorf("manifest %q declares no `requires:`; every app and widget is a capability resource (`requires: \"app:%s\"`), or the shell can never decide whether to draw it", m.id, m.id)
			continue
		}
		if m.requires != "app:"+m.id {
			t.Errorf("manifest %q requires %q; an app's own door is `app:<id>` and the seeds name it that way", m.id, m.requires)
		}
		out[m.requires] = true
		for _, s := range m.sections {
			if s.requires == "" {
				continue
			}
			if !strings.HasPrefix(s.requires, "app:"+m.id+"/") {
				t.Errorf("manifest %q section %q requires %q; a section's resource is `app:<id>/<section>` under its own app", m.id, s.id, s.requires)
			}
			out[s.requires] = true
		}
	}
	return out
}

func TestOsRegistryRequiresMatchTheAppSeeds(t *testing.T) {
	_, seeded := seededAppResources(t)
	registry := osRegistryResources(t)

	// THE REACHABLE POSITIVE, on both sides. Two empty maps compare equal, and
	// that is the exact failure mode this gate exists to refuse.
	if len(registry) == 0 {
		t.Fatalf("read no manifests out of %s; the registry moved or changed shape, and this gate would otherwise pass on nothing", osRegistryPath)
	}
	if len(seeded) == 0 {
		t.Fatal("the seeds carry no `read app:*` capability; the app block of dsl/rbac/seeds.memql is gone")
	}

	for _, resource := range sortedResourceNames(registry) {
		if _, ok := seeded[resource]; !ok {
			t.Errorf("the OS registry requires %s and dsl/rbac/seeds.memql seeds no `read` on it.\n"+
				"Add one cap-<role>-read-%s row per role that may open it, or the surface is hidden from everybody: the shell draws what the effective set holds, and no role holds a name the catalog never seeded.",
				resource, strings.NewReplacer(":", "-", "/", "-").Replace(resource))
		}
	}
	for _, resource := range sortedResourceKeys(seeded) {
		if !registry[resource] {
			t.Errorf("dsl/rbac/seeds.memql seeds `read` on %s for %v and the OS registry has no manifest or section requiring it.\n"+
				"A seeded resource the shell never asks for gates nothing and drifts; take the rows out or add the surface.",
				resource, seeded[resource])
		}
	}
}

// TestKnownResourcesListEveryAppAfterTheSeedsAreInstalled is task memql#5300's
// third criterion: once the catalog built from the seeds is installed, the
// @requiresCapability vocabulary names every app and every part -- which is
// what lets a construct declare one and load.
func TestKnownResourcesListEveryAppAfterTheSeedsAreInstalled(t *testing.T) {
	installSeededCatalog(t)
	e := &MemQLEngine{}
	assertKnownResourcesNameEveryApp(t, e.knownResources(context.Background()))
}

// TestKnownResourcesListEveryAppOnAFirstBoot is the case the catalog cannot
// answer: no catalog is installed because the rows it is built from are
// being materialized by this very startup, and the vocabulary must come from
// the seed DECLARATIONS the tree loaded -- or every part annotation refuses
// load on a fresh database and loads fine on one that has booted before.
func TestKnownResourcesListEveryAppOnAFirstBoot(t *testing.T) {
	auth.SetCapabilityCatalog(nil)
	reg := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(slog.New(slog.NewTextHandler(io.Discard, nil)), reg); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}
	e := &MemQLEngine{seeds: reg}
	assertKnownResourcesNameEveryApp(t, e.knownResources(context.Background()))
}

func assertKnownResourcesNameEveryApp(t *testing.T, known []string) {
	t.Helper()
	for _, resource := range sortedResourceNames(osRegistryResources(t)) {
		if !vocabularyHas(known, resource) {
			t.Errorf("knownResources does not list %s", resource)
		}
	}
	for _, part := range []string{"sources", "deploy", "publish", "retire", "domains"} {
		if !vocabularyHas(known, "app:deployables/"+part) {
			t.Errorf("knownResources does not list app:deployables/%s; the Deployables part is not seeded on any role", part)
		}
	}
	// And the core vocabulary is still there beside them.
	if !vocabularyHas(known, auth.ResourcePrincipal) {
		t.Errorf("knownResources lost the core vocabulary: %v", known)
	}
}

// TestAMisspelledPartRefusesLoadNamingTheKnownParts is task memql#5301's
// second criterion, through the same load-time check the tree walk runs. The
// construct is a BUILTIN parsed from source, because that is where the
// Deployables parts are declared and because a builtin's annotation reaches
// Function.RequiresCapability by a different path (builtin_converter.go)
// than a mutation's does.
func TestAMisspelledPartRefusesLoadNamingTheKnownParts(t *testing.T) {
	installSeededCatalog(t)
	e := &MemQLEngine{}
	registry := newFunctionRegistry()

	fn := parseBuiltinForTest(t, `@sdk
@executor("integration.packages.probe")
@args(profile="object")
@requiresCapability("execute", "app:deployables/publsh")
builtin misspelledPartProbe {
  packageId  string!  @description("The package.")
}
`)
	if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/publsh"}) {
		t.Fatalf("the builtin's @requiresCapability did not reach the Function: %+v", fn.RequiresCapability)
	}
	_ = registry.Upsert(fn)

	problems := e.validateRequiresCapabilitySlugs(context.Background(), registry)
	if len(problems) != 1 {
		t.Fatalf("want exactly one load problem for the misspelled part, got %d: %v", len(problems), problems)
	}
	msg := problems[0].Error()
	for _, want := range []string{"misspelledPartProbe", "app:deployables/publsh", "app:deployables/publish", "app:deployables/deploy", "app:deployables/retire"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name %q so the author does not grep the seeds: %s", want, msg)
		}
	}
}

// TestTheDeployablesPartsAreDeclaredOnTheirConstructs reads the shipped
// platform tree and holds the mapping the design record's table states: each
// Deployables action carries exactly the part it belongs to, and the shared
// reads carry none.
func TestTheDeployablesPartsAreDeclaredOnTheirConstructs(t *testing.T) {
	want := map[string]string{
		// sources
		"createPackage":          "app:deployables/sources",
		"updatePackageSource":    "app:deployables/sources",
		"setPackageAutoDeploy":   "app:deployables/sources",
		"packageSetAutoDeploy":   "app:deployables/sources",
		"sourceCredentialCreate": "app:deployables/sources",
		"sourceCredentialRevoke": "app:deployables/sources",
		// deploy
		"packageAnalyze":          "app:deployables/deploy",
		"packageDeploy":           "app:deployables/deploy",
		"packageCancelDeployment": "app:deployables/deploy",
		"sitePublishFromArtifact": "app:deployables/deploy",
		// publish
		"updateSiteStatus": "app:deployables/publish",
		"packageRollback":  "app:deployables/publish",
		// retire
		"packageDeactivateDeployable": "app:deployables/retire",
		"disablePackageDeployables":   "app:deployables/retire",
		"enablePackageDeployables":    "app:deployables/retire",
		"siteArchive":                 "app:deployables/retire",
		"siteRestore":                 "app:deployables/retire",
		"packageArchive":              "app:deployables/retire",
		"packageRestore":              "app:deployables/retire",
		"siteDelete":                  "app:deployables/retire",
		"deleteSite":                  "app:deployables/retire",
		// domains
		"customDomainAdd":    "app:deployables/domains",
		"removeCustomDomain": "app:deployables/domains",
	}
	// The shared reads stay unannotated (D8).
	unannotated := []string{"sitesAll", "siteById", "packagesAll", "packageById", "packageDeployments"}

	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fns := newFunctionRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, _, err := LoadUnifiedFunctions(logger, fns, memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if _, err := LoadUnifiedBuiltins(logger, fns); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	for name, resource := range want {
		fn, err := fns.Get(name)
		if err != nil || fn == nil {
			t.Errorf("%s is not in the loaded tree: %v", name, err)
			continue
		}
		if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: resource}) {
			t.Errorf("%s declares %v, want execute on %s", name, fn.RequiresCapability, resource)
		}
	}
	for _, name := range unannotated {
		fn, err := fns.Get(name)
		if err != nil || fn == nil {
			t.Errorf("%s is not in the loaded tree: %v", name, err)
			continue
		}
		if fn.RequiresCapability.declared() {
			t.Errorf("%s is a read shared by several apps and must carry no app annotation (D8); it declares %v", name, fn.RequiresCapability)
		}
	}
}

// ---------------------------------------------------------------------
// the seeds, through the real loader and the real catalog builder
// ---------------------------------------------------------------------

// seededLadder is the role ladder as the seeds declare it: slug -> rank,
// plus alias -> slug. What `min: "admin"` admits is a question about THIS
// ladder, and reading it from the seeds rather than restating it keeps the
// gate about two files rather than three.
type seededLadder struct {
	rank    map[string]int
	aliases map[string]string
}

func (l seededLadder) sortByRank(slugs []string) {
	sort.Slice(slugs, func(i, j int) bool {
		if l.rank[slugs[i]] != l.rank[slugs[j]] {
			return l.rank[slugs[i]] > l.rank[slugs[j]]
		}
		return slugs[i] < slugs[j]
	})
}

// seededAppResources loads dsl/rbac/seeds.memql through the real loader and
// answers the ladder plus, per `app:*` resource, the sorted slugs holding
// `read` on it -- so a seeded deny (none today) would subtract exactly as it
// does at runtime.
func seededAppResources(t *testing.T) (seededLadder, map[string][]string) {
	t.Helper()
	cat, ladder := seededCatalog(t)
	out := map[string][]string{}
	for _, slug := range cat.Slugs() {
		for _, vr := range cat.Grants(slug) {
			if vr.Verb != auth.VerbRead || !strings.HasPrefix(vr.Resource, "app:") {
				continue
			}
			out[vr.Resource] = append(out[vr.Resource], slug)
		}
	}
	for resource := range out {
		ladder.sortByRank(out[resource])
	}
	return ladder, out
}

// seededCatalog lowers the loaded seed definitions into the same rows the
// materializer writes and hands them to buildRbacCatalog -- the production
// builder -- so the catalog under test is the one a booted node installs.
func seededCatalog(t *testing.T) (*rbacCatalog, seededLadder) {
	t.Helper()
	reg := NewSeedRegistry()
	report := newLoadReport()
	if _, err := LoadUnifiedSeeds(slog.New(slog.NewTextHandler(io.Discard, nil)), reg, report); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}
	// A seed that fails to parse is SKIPPED by the loader and reported, and a
	// skipped capability row is a grant that never reaches the catalog -- so
	// a broken row here would read as a missing grant rather than as a
	// broken file. Refuse the skips instead.
	if report.HasProblems() {
		t.Fatalf("the seed loader reported problems, and a skipped capability row is a grant that never reaches the catalog:\n%s", report.Detail())
	}
	ladder := seededLadder{rank: map[string]int{}, aliases: map[string]string{}}
	var roles []roleRow
	var caps []capabilityRow
	for _, def := range reg.All() {
		if !strings.Contains(def.Origin, "rbac/seeds.memql") {
			continue
		}
		f := def.Body.fields
		switch def.UseConcept {
		case "role":
			slug := f["slug"].str
			ladder.rank[slug] = int(f["rank"].intV)
			for _, a := range f["aliases"].stringsV {
				ladder.aliases[a] = slug
			}
			roles = append(roles, roleRow{id: "v1:rbac:role:" + def.Name, slug: slug, name: f["name"].str, rank: int(f["rank"].intV), active: true, aliases: f["aliases"].stringsV, predefined: true})
		case "capability":
			effect := f["effect"].str
			if effect == "" {
				effect = "allow"
			}
			caps = append(caps, capabilityRow{id: "v1:rbac:capability:" + def.Name, roleSlug: f["roleSlug"].str, verb: f["verb"].str, resource: f["resourceType"].str, effect: effect, active: true})
		}
	}
	if len(roles) == 0 || len(caps) == 0 {
		t.Fatalf("the seed loader answered %d roles and %d capabilities; the rbac seeds moved or the binding changed", len(roles), len(caps))
	}
	return buildRbacCatalog(roleNodes(roles...), capabilityNodes(caps...)), ladder
}

// installSeededCatalog installs the catalog the seeds describe as THE
// catalog for one test.
func installSeededCatalog(t *testing.T) {
	t.Helper()
	cat, _ := seededCatalog(t)
	auth.SetCapabilityCatalog(cat)
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

// ---------------------------------------------------------------------
// the registry, read as text
// ---------------------------------------------------------------------

type osSection struct {
	id string
	// requires is the section's own capability resource, "" when it is
	// reached through its app's door.
	requires string
}

type osManifest struct {
	id       string
	requires string
	sections []osSection
}

// osRegistrySource is registry.tsx with its comments stripped, plus the
// import map it declares, so a manifest or section list defined in a
// sibling file (`sections: DEPLOYABLES_SECTIONS`, `setupWidget`) can be
// followed to where it lives.
type osRegistrySource struct {
	dir     string
	src     string
	imports map[string]string // identifier -> file path
	files   map[string]string // file path -> comment-stripped source
}

var (
	osImportRe   = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*"(\./[^"]+)"`)
	osRegistryRe = regexp.MustCompile(`export const OS_REGISTRY: OsRegistry = `)
	osIdRe       = regexp.MustCompile(`id:\s*"([^"]+)"`)
	osRequiresRe = regexp.MustCompile(`requires:\s*"([^"]+)"`)
	osRolesRe    = regexp.MustCompile(`\broles:`)
)

func newOsRegistrySource(t *testing.T, path string) *osRegistrySource {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the OS app registry is unreadable at %s: %v\n"+
			"This gate is what keeps the app seeds and the registry floors in step. If the file moved, update osRegistryPath; if it was deleted, the OS lists its apps some other way and that needs a decision, not a skip.",
			path, err)
	}
	r := &osRegistrySource{dir: filepath.Dir(path), src: stripTsComments(string(raw)), imports: map[string]string{}, files: map[string]string{}}
	for _, m := range osImportRe.FindAllStringSubmatch(r.src, -1) {
		for _, name := range strings.Split(m[1], ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			r.imports[name] = r.resolveImport(t, m[2])
		}
	}
	return r
}

func (r *osRegistrySource) resolveImport(t *testing.T, rel string) string {
	t.Helper()
	base := filepath.Join(r.dir, rel)
	for _, ext := range []string{".ts", ".tsx"} {
		if _, err := os.Stat(base + ext); err == nil {
			return base + ext
		}
	}
	t.Fatalf("registry import %q resolves to no .ts or .tsx file under %s", rel, r.dir)
	return ""
}

func (r *osRegistrySource) file(t *testing.T, path string) string {
	t.Helper()
	if s, ok := r.files[path]; ok {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	r.files[path] = stripTsComments(string(raw))
	return r.files[path]
}

// manifests walks OS_REGISTRY's `apps` and `widgets` lists, in order, and
// reads each named manifest -- in registry.tsx, or in the file it was
// imported from.
func (r *osRegistrySource) manifests(t *testing.T) []osManifest {
	t.Helper()
	loc := osRegistryRe.FindStringIndex(r.src)
	if loc == nil {
		t.Fatalf("%s does not export OS_REGISTRY as `export const OS_REGISTRY: OsRegistry = {...}`", osRegistryPath)
	}
	registry := tsBlock(t, r.src, loc[1])
	var out []osManifest
	for _, key := range []string{"apps", "widgets"} {
		list, ok := tsTopLevelField(t, registry, key)
		if !ok {
			t.Fatalf("OS_REGISTRY has no `%s:` list", key)
		}
		for _, ident := range strings.Split(strings.Trim(strings.TrimSpace(list), "[]"), ",") {
			ident = strings.TrimSpace(ident)
			if ident == "" {
				continue
			}
			out = append(out, r.manifest(t, ident))
		}
	}
	return out
}

func (r *osRegistrySource) manifest(t *testing.T, ident string) osManifest {
	t.Helper()
	re := regexp.MustCompile(`const ` + regexp.QuoteMeta(ident) + `: Os(App|Widget)Manifest = `)
	sources := []string{r.src}
	if path, ok := r.imports[ident]; ok {
		sources = append(sources, r.file(t, path))
	}
	for _, src := range sources {
		loc := re.FindStringIndex(src)
		if loc == nil {
			continue
		}
		block := tsBlock(t, src, loc[1])
		idField, ok := tsTopLevelField(t, block, "id")
		if !ok {
			t.Fatalf("manifest %s declares no id", ident)
		}
		m := osManifest{id: r.resolveId(t, strings.TrimSpace(idField))}
		// THE RETIRED FORM IS REFUSED, not skipped: a `roles:` floor the shell
		// no longer reads is one that gates nothing while looking like a gate.
		if _, ok := tsTopLevelField(t, block, "roles"); ok {
			t.Errorf("manifest %s still carries `roles:`; the shell reads `requires: \"app:<id>\"` (epic memql#5289) and a floor written here gates nothing", ident)
		}
		if reqField, ok := tsTopLevelField(t, block, "requires"); ok {
			m.requires = parseOsRequires(t, ident, reqField)
		}
		if sec, ok := tsTopLevelField(t, block, "sections"); ok {
			sec = strings.TrimSpace(sec)
			if strings.HasPrefix(sec, "[") {
				m.sections = parseOsSections(t, sec)
			} else {
				path, ok := r.imports[sec]
				if !ok {
					t.Fatalf("manifest %s names sections %s, which registry.tsx does not import", ident, sec)
				}
				src2 := r.file(t, path)
				re2 := regexp.MustCompile(`export const ` + regexp.QuoteMeta(sec) + `(?::\s*OsAppSection\[\])?\s*=\s*`)
				loc2 := re2.FindStringIndex(src2)
				if loc2 == nil {
					t.Fatalf("%s does not export %s as a literal array", path, sec)
				}
				m.sections = parseOsSections(t, tsBlock(t, src2, loc2[1]))
			}
		}
		return m
	}
	t.Fatalf("no `const %s: OsAppManifest = {...}` (or OsWidgetManifest) in registry.tsx or the file it imports %s from", ident, ident)
	return osManifest{}
}

// resolveId answers a manifest's id: a string literal, or an imported
// constant (`id: BIN_APP_ID`) read from its file.
func (r *osRegistrySource) resolveId(t *testing.T, field string) string {
	t.Helper()
	if strings.HasPrefix(field, `"`) {
		return strings.Trim(field, `"`)
	}
	path, ok := r.imports[field]
	if !ok {
		t.Fatalf("manifest id %s is neither a literal nor an imported constant", field)
	}
	re := regexp.MustCompile(`export const ` + regexp.QuoteMeta(field) + `\s*=\s*"([^"]+)"`)
	m := re.FindStringSubmatch(r.file(t, path))
	if m == nil {
		t.Fatalf("%s does not export %s as a string literal", path, field)
	}
	return m[1]
}

// parseOsRequires reads a `requires:` value, which must be one string literal
// naming a resource. The module lists that used to be spelled `requires:`
// are `needs:` now, and a list here is the one shape this would misread.
func parseOsRequires(t *testing.T, where, txt string) string {
	t.Helper()
	txt = strings.TrimSpace(txt)
	if !strings.HasPrefix(txt, `"`) {
		t.Fatalf("%s: a `requires:` value this gate cannot read: %s (a resource is one string literal; module lists are `needs:`)", where, txt)
	}
	return strings.Trim(txt, `"`)
}

// parseOsSections reads the `{ id, name, requires? }` entries of a section
// array literal.
func parseOsSections(t *testing.T, arr string) []osSection {
	t.Helper()
	var out []osSection
	i := 1
	for {
		j := strings.Index(arr[i:], "{")
		if j < 0 {
			break
		}
		entry := tsBlock(t, arr, i+j)
		idm := osIdRe.FindStringSubmatch(entry)
		if idm == nil {
			t.Fatalf("a section entry with no id: %s", entry)
		}
		s := osSection{id: idm[1]}
		if osRolesRe.MatchString(entry) {
			t.Errorf("section %s still carries `roles:`; a section's floor is `requires: \"app:<id>/<section>\"` now (epic memql#5289)", idm[1])
		}
		if m := osRequiresRe.FindStringSubmatch(entry); m != nil {
			s.requires = m[1]
		}
		out = append(out, s)
		i = i + j + len(entry)
	}
	return out
}

// tsTopLevelField answers the value of `<name>:` written at a block's own
// indentation (two spaces), so a nested section's `roles:` is never read as
// the manifest's.
func tsTopLevelField(t *testing.T, block, name string) (string, bool) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:\s*`)
	loc := re.FindStringIndex(block)
	if loc == nil {
		return "", false
	}
	rest := block[loc[1]:]
	if strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "[") {
		return tsBlock(t, block, loc[1]), true
	}
	end := strings.IndexAny(rest, ",\n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end], true
}

// tsBlock answers the brace- or bracket-balanced block starting at src[start],
// string literals respected.
func tsBlock(t *testing.T, src string, start int) string {
	t.Helper()
	if start >= len(src) {
		t.Fatalf("block start past end of source")
	}
	open := src[start]
	var closeCh byte
	switch open {
	case '{':
		closeCh = '}'
	case '[':
		closeCh = ']'
	default:
		t.Fatalf("expected a block at %q", src[start:min(start+20, len(src))])
	}
	depth := 0
	var inq byte
	for i := start; i < len(src); i++ {
		c := src[i]
		switch {
		case inq != 0:
			if c == '\\' {
				i++
			} else if c == inq {
				inq = 0
			}
		case c == '"' || c == '\'' || c == '`':
			inq = c
		case c == open:
			depth++
		case c == closeCh:
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced block starting at %q", src[start:min(start+40, len(src))])
	return ""
}

var tsBlockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

// stripTsComments removes block comments and `//` line comments, leaving
// string literals alone, so a floor DISCUSSED in a comment -- and the
// registry discusses them at length -- is never read as a declaration.
func stripTsComments(s string) string {
	s = tsBlockCommentRe.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	for n, line := range lines {
		var inq byte
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case inq != 0:
				if c == '\\' {
					i++
				} else if c == inq {
					inq = 0
				}
			case c == '"' || c == '\'' || c == '`':
				inq = c
			case c == '/' && i+1 < len(line) && line[i+1] == '/':
				line = line[:i]
				i = len(line)
			}
		}
		lines[n] = line
	}
	return strings.Join(lines, "\n")
}

func sortedResourceKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedResourceNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseBuiltinForTest parses one builtin declaration from struct-form source
// through the real parser and converter.
func parseBuiltinForTest(t *testing.T, source string) *Function {
	t.Helper()
	decl, err := languageParser.ParseBuiltinDecl(source)
	if err != nil {
		t.Fatalf("parse builtin: %v", err)
	}
	fn, err := builtinDeclToFunction(decl, "app-resource-test")
	if err != nil {
		t.Fatalf("convert builtin: %v", err)
	}
	return fn
}
