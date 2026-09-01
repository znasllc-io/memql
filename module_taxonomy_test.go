package main

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// MemQL has exactly three extension words -- component, integration, pack --
// and docs/public/concepts/component-integration-pack.md gives the test
// verbatim: an integration "talks to somebody else's system; THAT IS THE WHOLE
// TEST".
//
// The portal's Integrations page does not apply it. `integrationStatus` builds
// its list from `memql.RegisteredPlugins()` raw, and RegisterPlugin is the
// DSL->Go capability seam EVERY Go-backed builtin uses -- so the page shows the
// seam and calls it the taxonomy. Sixteen of the twenty-seven names on it talk
// to no external system at all (memql#4276).
//
// THIS GATE IS AGENDA ITEM 4 OF THAT EPIC, and only item 4. It does not move a
// package, retire a page or change what /integrations renders. What it does is
// make the kind decision UNSKIPPABLE: a new RegisterPlugin name that is not
// classified below fails the build, so the next one is decided at PR time by
// the person adding it rather than defaulted into the integration bucket by a
// page that cannot tell. The reasoning behind each row, and the work item 4
// does not do, is in docs/internal/design/module-taxonomy.md.
//
// WHY A STATIC SCAN AND NOT RegisteredPlugins(). The registry is populated by
// `init()` in each integration package, so a test only sees the plugins its own
// binary imports -- and this test's binary is the root package, which does not
// import the node-type-scoped ones. Scanning the source finds every
// registration in the tree regardless of build tags, which is the population
// the question is actually about.

// moduleKind is the three-word vocabulary. `nodeType` exists in
// component/memql's module registry as a fourth ENUMERATION, but it is not an
// extension word and nothing registers a plugin as one.
type moduleKind string

const (
	kindComponent   moduleKind = "component"   // engine internals; cannot be switched off
	kindIntegration moduleKind = "integration" // talks to somebody else's system
	kindPack        moduleKind = "pack"        // a product feature an operator may switch off
)

// pluginKinds is the maintained table. Every `memql.RegisterPlugin` name in the
// tree must appear here, and every name here must still be registered.
//
// The verdicts are the epic's, checked against the source rather than inherited
// from it -- the four it left open are resolved at the bottom, each with what
// decided it.
var pluginKinds = map[string]moduleKind{
	// --- TRUE INTEGRATIONS: an outbound call to somebody else's system. ---
	"avatardirect":   kindIntegration, // Simli / Anam + LiveKit
	"email":          kindIntegration, // Microsoft Graph / SMTP
	"openairealtime": kindIntegration, // api.openai.com
	"release":        kindIntegration, // api.github.com + ghcr.io -- tags, Releases, manifests
	"shopify":        kindIntegration, // Storefront + Admin APIs
	"storage":        kindIntegration, // Azure Blob
	"telephony":      kindIntegration, // Telnyx + LiveKit
	"voice":          kindIntegration, // LiveKit

	// --- COMPONENTS: engine internals an operator cannot switch off. ---
	// Turning any of these off does not remove a feature, it breaks the
	// engine: there is no MemQL that does not authorise, route or read rows.
	"auth":          kindComponent,
	"rbac":          kindComponent,
	"router":        kindComponent,
	"database":      kindComponent,
	"identity":      kindComponent,
	"timeutil":      kindComponent,
	"embedding":     kindComponent, // see the note below
	"deployversion": kindComponent,
	"sitePublish":   kindComponent, // see the note below
	// Custom domains (epic memql#4805). A COMPONENT by this table's own test:
	// does turning it off remove a feature, or break the engine?
	//
	// It breaks it. The two builtins are declared in dsl/platform/builtins.memql,
	// which EVERY binary loads, and a capability present in the DSL and absent
	// from the registry is a boot-time resolution failure -- the same reason
	// `release` above is registered on every node type. The sweep automation
	// loads everywhere too.
	//
	// It is also not an integration: it calls nobody's API. It reads public
	// DNS and this cluster's own Kubernetes API server, which is the same
	// relationship `database` has to Postgres rather than the one `shopify`
	// has to Shopify.
	"customDomain": kindComponent,
	// The data-origins runtime (epic memql#4378): the inbound dispatcher
	// and the backfill / reconciliation runners.
	//
	// A COMPONENT rather than a pack, and the test is the one this table
	// states -- does turning it off remove a feature, or break the engine?
	// It breaks it. The dispatcher automation loads on every binary, so a
	// node without the executor fails every inbound row from EVERY source,
	// not just the connectors'. And the invariants the runtime serves are
	// engine invariants: a mirror is read-only whether or not anything is
	// filling it, and the outbox is appended in the write's transaction
	// whether or not anything drains it.
	//
	// It is also not an integration: it calls nobody. It drives whichever
	// connectors are bound, and THOSE are the integrations (shopify is
	// classified as one above).
	//
	// The operator switch that does exist, MEMQL_SYNC_ENABLED, stops
	// DELIVERY and nothing else -- which is the shape of a component's
	// tunable, not of a pack's on/off.
	"datasync": kindComponent,

	// --- PACKS: product features with a coherent "off". ---
	"chat":          kindPack,
	"dailyspace":    kindPack,
	"agents":        kindPack,
	"library":       kindPack,
	"knowledge":     kindPack, // see the note below
	"liveknowledge": kindPack,
	"actionSearch":  kindPack,
	"similarity":    kindPack,
	"files":         kindPack,
	"workbench":     kindPack, // see the note below
	"harnessRecall": kindPack, // already BindPluginToPack("harness")
	"harnessTrace":  kindPack, // already BindPluginToPack("harness")
}

// The four the epic left open, and what decided each. Recorded here rather than
// only in the design doc because the evidence is in this repository and a
// reader disagreeing with a row should be able to re-run the check.
//
//   - embedding -> COMPONENT. It makes no outbound call: integrations/embedding
//     resolves an EmbeddingAIProvider from the engine's own provider registry
//     (plugin.go:20-42) and the PROVIDER makes the vendor call. It is
//     infrastructure the engine's own search depends on, and the vendor lane it
//     rides is already switchable through a provider's @disabled.
//
//   - dailyspace -> PACK. No HTTP anywhere in the package. A product feature
//     with a coherent "off" -- there is a user preference for it already.
//
//   - workbench -> PACK. It holds an http.Client, and that is NOT the doc's
//     test being met. `handleHTTPFetch` fetches a URL the model chooses at call
//     time; the system on the other end is not integrated with, it is browsed.
//     A connector knows whose API it is speaking. This one does not, which is
//     precisely why it is a sandboxed capability slug and not a vendor lane.
//
//   - sitePublish -> COMPONENT (memql#4345). It makes no outbound call to
//     anybody's system: it reads the cluster's OWN object storage and writes
//     the cluster's own v1:platform:site row through component/edge's
//     Publisher -- the same Publisher POST /sites/{id}/bundles uses. Site
//     hosting is engine machinery rather than a switchable product feature:
//     the platform's own console is site #1, so there is no MemQL with this
//     spine switched off. It registers separately from `library` -- and no
//     longer shares its package: it lives in component/sitepublish, in the ROOT
//     module, because component/edge is in the root module too and integrations
//     is a separate module the root already requires, so importing edge from
//     integrations/library made the module graph a cycle. CI's
//     module-boundaries lane is what says so; the go.work workspace resolves
//     the import and hides it.
//
//   - knowledge -> PACK, with a real integration inside it. This is the one
//     genuinely mixed case: integrations/knowledge/seed_wikipedia.go DOES call
//     Wikipedia's REST API, with its own User-Agent and http.Client, which is
//     the doc's test met exactly. The package as a whole is a product feature
//     (knowledge domains); the seeder is a connector living inside it. That is
//     the Shopify shape the epic names -- an integration plus a pack on top --
//     and splitting it is work this gate does not do. Recorded so the next
//     reader knows the row is a summary of a package that contains both.
var _ = pluginKinds

// rePluginRegistration finds every registration in the tree. The name is always
// a string literal -- a computed one could not be classified at PR time, which
// is the property this gate exists to keep.
var rePluginRegistration = regexp.MustCompile(`memql\.RegisterPlugin\(\s*"([A-Za-z0-9_]+)"`)

// TestEveryRegisteredPluginIsClassified is the gate.
func TestEveryRegisteredPluginIsClassified(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	found := map[string]string{} // name -> first file that registers it
	var scanned int
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") || strings.Contains(rel, "/node_modules/") {
			continue
		}
		body, err := os.ReadFile(rel)
		if err != nil {
			continue
		}
		scanned++
		for _, m := range rePluginRegistration.FindAllStringSubmatch(string(body), -1) {
			if _, seen := found[m[1]]; !seen {
				found[m[1]] = rel
			}
		}
	}

	// COVERAGE FLOOR, both halves. A scan that read no files, or read them and
	// matched nothing, must not report a pass -- the registrations are the
	// population this gate is about, and finding none of them means the regexp
	// or the walk is broken, not that the tree is clean.
	if scanned < 500 {
		t.Fatalf("scanned only %d Go files -- this gate is not looking at the repository", scanned)
	}
	if len(found) < 20 {
		t.Fatalf("found only %d plugin registrations; the tree has ~27. Either the walk or "+
			"rePluginRegistration is broken, and a pass would mean nothing", len(found))
	}

	for name, file := range found {
		if _, ok := pluginKinds[name]; ok {
			continue
		}
		t.Errorf("%s registers the plugin %q, which pluginKinds does not classify.\n"+
			"    RegisterPlugin is the DSL->Go capability seam, NOT the taxonomy: the portal's\n"+
			"    /integrations page lists every registered name and calls them all integrations,\n"+
			"    and sixteen of the existing ones talk to no external system at all (memql#4276).\n"+
			"    Decide the kind now, while you know the answer:\n"+
			"      integration -- it talks to somebody else's system. That is the whole test.\n"+
			"      component   -- engine internals; there is no MemQL with this switched off.\n"+
			"      pack        -- a product feature an operator may switch off (and then\n"+
			"                     BindPluginToPack, so the packState toggle works for free).\n"+
			"    Add a row to pluginKinds with a one-line reason. Background:\n"+
			"    docs/internal/design/module-taxonomy.md",
			file, name)
	}

	// The other direction: a classified name nobody registers is a row that has
	// outlived its plugin, and it makes the table look more complete than it is.
	var stale []string
	for name := range pluginKinds {
		if _, ok := found[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("pluginKinds classifies %v, which nothing in the tree registers any more. "+
			"Remove the rows -- a table listing plugins that do not exist reads as coverage "+
			"it does not have.", stale)
	}
}

// TestModuleKindsAreTheThreeExtensionWords stops the vocabulary growing.
//
// "MemQL has exactly three extension words. Do not invent a fourth" is
// CLAUDE.md's rule, and the failure mode it guards is not someone declaring a
// fourth const -- it is someone reaching for a plausible new word (`service`,
// `provider`, `connector`) for a case that did not fit, and the taxonomy
// becoming four words nobody can define.
func TestModuleKindsAreTheThreeExtensionWords(t *testing.T) {
	allowed := map[moduleKind]bool{kindComponent: true, kindIntegration: true, kindPack: true}
	seen := map[moduleKind]int{}
	for name, kind := range pluginKinds {
		if !allowed[kind] {
			t.Errorf("%q is classified %q, which is not one of the three extension words "+
				"(component / integration / pack). CLAUDE.md: do not invent a fourth.", name, kind)
		}
		seen[kind]++
	}
	// Each word must actually be used. A vocabulary with an unused word is one
	// where the distinction has quietly collapsed.
	for kind := range allowed {
		if seen[kind] == 0 {
			t.Errorf("no plugin is classified %q -- either the table is wrong or the word is", kind)
		}
	}
}
