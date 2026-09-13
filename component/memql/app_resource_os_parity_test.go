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
// memql#5288, task memql#5302; app access grants design, sections 4 and 5).
//
// dsl/rbac/seeds.memql seeds `read app:<id>` for every MemQL OS app on
// exactly the roles the registry's hand-written `roles:` floor admits today,
// and `read app:<id>/<section>` for every floored section. That is the whole
// promise of the migration: the day the shell stops asking `roleAdmits(role,
// { min })` and asks the effective capability set instead (epic memql#5289),
// nobody's desktop changes. A promise like that is worth exactly the gate
// that holds it, so this reads the registry SOURCE -- the way
// TestSiteKindEnumMatchesOsOfferedKinds reads the offered kinds -- and fails
// the build when a floor and its seeded role set differ in EITHER direction:
//
//   - an app (or floored section) the registry has and the seeds do not is
//     a surface the switch would take away from everybody;
//   - a seeded app the registry does not have is a resource that gates
//     nothing and will drift;
//   - a role set that differs is a person who gains or loses an app on the
//     day of the switch, silently.
//
// WRITTEN SO THE OS EPIC CHANGES ONE FUNCTION. osRegistryResources is the
// extraction: today it reads each entry's `roles:` floor and derives the
// admitted set from the seeded ladder; the OS epic replaces its body with
// one that reads `requires: "app:<id>"` and asserts every named resource is
// seeded. The comparison below does not change.
//
// The seeds are read through the REAL loader (LoadUnifiedSeeds) and lowered
// through the REAL catalog builder, so what is compared is the catalog a
// booted node installs, not a regex over the file.

// osRegistryPath is the MemQL OS app registry, relative to this package.
const osRegistryPath = "../../clients/os/src/apps/registry.tsx"

// osRegistryResources reads every app and widget manifest the registry lists
// and answers, per capability resource, the sorted catalog slugs the
// registry's own floor admits: `app:<id>` for the manifest and
// `app:<id>/<section>` for every section that declares a floor of its own.
//
// A section with NO floor of its own is not a resource: it is reached
// through its app's door, and seeding it would be a second row saying what
// the app row already says.
func osRegistryResources(t *testing.T, ladder seededLadder) map[string][]string {
	t.Helper()
	reg := newOsRegistrySource(t, osRegistryPath)
	out := map[string][]string{}
	for _, m := range reg.manifests(t) {
		out["app:"+m.id] = ladder.admitted(t, m.roles)
		for _, s := range m.sections {
			if s.roles == nil {
				continue
			}
			out["app:"+m.id+"/"+s.id] = ladder.admitted(t, s.roles)
		}
	}
	return out
}

func TestOsRegistryFloorsMatchTheAppSeeds(t *testing.T) {
	ladder, seeded := seededAppResources(t)
	registry := osRegistryResources(t, ladder)

	// THE REACHABLE POSITIVE, on both sides. Two empty maps compare equal, and
	// that is the exact failure mode this gate exists to refuse.
	if len(registry) == 0 {
		t.Fatalf("read no manifests out of %s; the registry moved or changed shape, and this gate would otherwise pass on nothing", osRegistryPath)
	}
	if len(seeded) == 0 {
		t.Fatal("the seeds carry no `read app:*` capability; the app block of dsl/rbac/seeds.memql is gone")
	}

	for _, resource := range sortedResourceKeys(registry) {
		want := registry[resource]
		got, ok := seeded[resource]
		if !ok {
			t.Errorf("the OS registry admits %s on %v and dsl/rbac/seeds.memql seeds no `read` on it.\n"+
				"Add one cap-<role>-read-%s row per admitted role, or the switch to `requires:` takes the surface away from everybody.",
				resource, want, strings.NewReplacer(":", "-", "/", "-").Replace(resource))
			continue
		}
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("%s: the OS registry floor admits %v and the seeds grant `read` to %v.\n"+
				"These must be the SAME set: the seeds reproduce today's floors exactly, so that the day the shell reads the effective set instead of the floor, nobody's desktop changes.",
				resource, want, got)
		}
	}
	for _, resource := range sortedResourceKeys(seeded) {
		if _, ok := registry[resource]; !ok {
			t.Errorf("dsl/rbac/seeds.memql seeds `read` on %s for %v and the OS registry has no such app or floored section.\n"+
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
	ladder, _ := seededAppResources(t)
	for _, resource := range sortedResourceKeys(osRegistryResources(t, ladder)) {
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

// admitted answers the catalog slugs a registry floor admits, sorted by rank
// descending. nil (no floor) is every seeded role.
func (l seededLadder) admitted(t *testing.T, r *osRoleRequirement) []string {
	t.Helper()
	var out []string
	switch {
	case r == nil:
		for slug := range l.rank {
			out = append(out, slug)
		}
	case r.min != "":
		floor, ok := l.rank[l.resolve(r.min)]
		if !ok {
			t.Fatalf("the registry floor names %q, which the seeds neither seed nor alias", r.min)
		}
		for slug, rank := range l.rank {
			if rank >= floor {
				out = append(out, slug)
			}
		}
	default:
		for _, name := range r.any {
			slug := l.resolve(name)
			if _, ok := l.rank[slug]; !ok {
				t.Fatalf("the registry's `any` set names %q, which the seeds neither seed nor alias", name)
			}
			out = append(out, slug)
		}
	}
	l.sortByRank(out)
	return out
}

func (l seededLadder) resolve(name string) string {
	if slug, ok := l.aliases[name]; ok {
		return slug
	}
	return name
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

// osRoleRequirement is the registry's RoleRequirement: `{ min }` or `{ any }`.
type osRoleRequirement struct {
	min string
	any []string
}

type osSection struct {
	id    string
	roles *osRoleRequirement
}

type osManifest struct {
	id       string
	roles    *osRoleRequirement
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
	osMinRe      = regexp.MustCompile(`min:\s*"([^"]+)"`)
	osAnyRe      = regexp.MustCompile(`any:\s*\[([^\]]*)\]`)
	osQuotedRe   = regexp.MustCompile(`"([^"]+)"`)
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
		if rolesField, ok := tsTopLevelField(t, block, "roles"); ok {
			m.roles = parseOsRoles(t, rolesField)
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

func parseOsRoles(t *testing.T, txt string) *osRoleRequirement {
	t.Helper()
	if m := osMinRe.FindStringSubmatch(txt); m != nil {
		return &osRoleRequirement{min: m[1]}
	}
	if m := osAnyRe.FindStringSubmatch(txt); m != nil {
		var any []string
		for _, q := range osQuotedRe.FindAllStringSubmatch(m[1], -1) {
			any = append(any, q[1])
		}
		return &osRoleRequirement{any: any}
	}
	t.Fatalf("a `roles:` value this gate cannot read: %s", txt)
	return nil
}

// parseOsSections reads the `{ id, name, roles? }` entries of a section
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
		if k := strings.Index(entry, "roles:"); k >= 0 {
			s.roles = parseOsRoles(t, tsBlock(t, entry, k+len("roles:")+strings.Index(entry[k+len("roles:"):], "{")))
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
