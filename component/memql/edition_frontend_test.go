package memql

// edition_frontend_test.go -- each file is parsed through its domain's edition
// (epic memql#5356, task memql#5358; D4 and D6 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// An edition never forks the parser: its front end is a source step in front
// of the one core (component/language/parser/edition.go). These tests hold the
// loaders to that: every file is read through the front end of the edition
// ITS domain declares, so two editions load in one engine.

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/actions"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslgate"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// predicateHeader is the one spelling the synthetic edition 2099 knows and the
// current edition does not: `predicate NAME = ...` for what 2026 writes as
// `trait NAME = ...`.
var predicateHeader = regexp.MustCompile(`(?m)^([ \t]*)predicate([ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*=)`)

// registerPredicateEdition installs edition 2099 for one test and removes it
// when the test ends, leaving the front-end table as it found it.
func registerPredicateEdition(t *testing.T) {
	t.Helper()
	unregister := langparser.RegisterEdition(langparser.FrontEnd{
		Edition:        "2099",
		GrammarVersion: "2099.01-synthetic-predicate",
		Prepare: func(src string) (string, error) {
			return predicateHeader.ReplaceAllString(src, "${1}trait${2}"), nil
		},
	})
	t.Cleanup(unregister)
}

// editionLine is a memql.toml declaring the engine's language line and the
// given edition.
func editionLine(edition string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(dslfs.Manifest{Language: langparser.LanguageVersion, Edition: edition}.Render())}
}

func traitSource(keyword, name string) []byte {
	return []byte(fmt.Sprintf("@enabled\n%s %s = row => row.active == true\n", keyword, name))
}

func registerEditionDomain(t *testing.T, domain, edition string, files fstest.MapFS) {
	t.Helper()
	files[dslfs.ManifestFile] = editionLine(edition)
	memqldsl.RegisterTree(domain, files)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })
}

// TestTwoEditionsLoadInOneEngine: domain alpha speaks 2026 and writes a trait;
// domain omega speaks 2099 and writes the same construct with the spelling
// only 2099 knows. One Init loads both.
func TestTwoEditionsLoadInOneEngine(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	registerPredicateEdition(t)
	registerEditionDomain(t, "alpha", "2026", fstest.MapFS{"traits.memql": {Data: traitSource("trait", "alphaEditionTrait")}})
	registerEditionDomain(t, "omega", "2099", fstest.MapFS{"traits.memql": {Data: traitSource("predicate", "omegaEditionTrait")}})

	eng := newQuietEngine(t)
	if err := eng.Init(loadedConceptRegistry(t)); err != nil {
		t.Fatalf("one engine must load a 2026 domain and a 2099 domain side by side: %v", err)
	}
	for _, name := range []string{"alphaEditionTrait", "omegaEditionTrait"} {
		if !eng.specs.Has(name) {
			t.Errorf("trait %q did not register; each file must be read through its own domain's edition", name)
		}
	}
	if line, ok := eng.LanguageLineFor("omega/traits.memql"); !ok || line.Edition != "2099" {
		t.Errorf("LanguageLineFor(omega/traits.memql) = %+v, %v; want edition 2099", line, ok)
	}

	// The same `predicate` source in a domain speaking 2026 is not a
	// construct there. The whole-file parse -- the offline load memqllint
	// runs -- refuses it, naming the keyword, while the identical root
	// declaring 2099 loads clean.
	predicate := traitSource("predicate", "alphaPredicateTrait")
	_, err := dslimports.Load(fstest.MapFS{
		"alpha/memql.toml":   editionLine("2026"),
		"alpha/traits.memql": {Data: predicate},
	})
	if err == nil || !strings.Contains(err.Error(), "predicate is not a construct keyword") {
		t.Errorf("under 2026 the predicate source must be refused as an unknown keyword, got: %v", err)
	}
	if _, err := dslimports.Load(fstest.MapFS{
		"omega/memql.toml":   editionLine("2099"),
		"omega/traits.memql": {Data: predicate},
	}); err != nil {
		t.Errorf("under 2099 the same source must load: %v", err)
	}

	// And at boot, a 2026 domain holding the same source is REFUSED:
	// `predicate` is not a construct keyword there. Beside it sits a trait
	// written in 2026 -- the positive control: it loads, so the domain was
	// read, and the refusal is the construct-keyword gate's rather than the
	// silence of a domain nobody read.
	control := traitSource("trait", "alphaPredicateControl")
	registerEditionDomain(t, "alphapredicate", "2026", fstest.MapFS{"traits.memql": {Data: append(control, predicate...)}})
	err = newQuietEngine(t).Init(loadedConceptRegistry(t))
	if err == nil {
		t.Fatal("strict boot loaded a 2026 domain opening a statement with `predicate`, a word no construct is spelled with")
	}
	for _, want := range []string{"strict DSL boot refused", "alphapredicate/traits.memql", "predicate is not a construct keyword", "[construct_unknown]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must carry %q, got:\n%v", want, err)
		}
	}
	t.Setenv(AllowSkipsEnvVar, "1")
	eng = newQuietEngine(t)
	if err := eng.Init(loadedConceptRegistry(t)); err != nil {
		t.Fatalf("Init under the break-glass: %v", err)
	}
	if !eng.specs.Has("alphaPredicateControl") {
		t.Error("positive control: the 2026 trait beside the refused statement did not load, so the domain was never read")
	}
	if eng.specs.Has("alphaPredicateTrait") {
		t.Error("a 2026 domain's predicate declaration registered a trait; only edition 2099 reads that spelling")
	}
}

// TestEveryParseSiteReadsThroughItsDomainsEdition holds each loader that reads
// the tree to the rule: a file reaches the parser only after the front end of
// the edition ITS domain declares. A spy edition counts the files it is handed;
// a loader that reads the file around it is a parse site this change missed.
// (component/automations, which cannot be imported from here, has its own.)
func TestEveryParseSiteReadsThroughItsDomainsEdition(t *testing.T) {
	const marker = "// edition-spy-marker"
	var mu sync.Mutex
	seen := 0
	unregister := langparser.RegisterEdition(langparser.FrontEnd{
		Edition:        "2098",
		GrammarVersion: "2098.01-spy",
		Prepare: func(src string) (string, error) {
			if strings.Contains(src, marker) {
				mu.Lock()
				seen++
				mu.Unlock()
			}
			return src, nil
		},
	})
	t.Cleanup(unregister)

	body := marker + "\n" + string(traitSource("trait", "editionSpyTrait"))
	const domain = "editionspy"
	registerEditionDomain(t, domain, "2098", fstest.MapFS{"traits.memql": {Data: []byte(body)}})
	// The same domain as a free-standing tree, for the loaders handed one.
	own := fstest.MapFS{
		domain + "/" + dslfs.ManifestFile: editionLine("2098"),
		domain + "/traits.memql":          {Data: []byte(body)},
	}

	sites := []struct {
		name string
		read func()
	}{
		{"baseloader.ReadAll", func() { baseloader.ReadAll(nil) }},
		{"BuildUnifiedConcepts", func() { _, _, _ = BuildUnifiedConcepts(nil, own) }},
		{"the capability-name loader", func() { _, _ = loadCapabilityNamesFromFS(own) }},
		{"the dependency-tree validator", func() { _ = validateDependencyTree(own) }},
		{"dslimports.Load", func() { _, _ = dslimports.Load(own) }},
		{"actions.Registry.LoadFromFS", func() { _, _ = actions.NewRegistry().LoadFromFS(own) }},
		{"actions.LoadCatalogFromFS", func() { _, _ = actions.LoadCatalogFromFS(own) }},
		{"dslgate.ScanTree", func() { _, _ = dslgate.ScanTree(own, dslgate.Options{}) }},
	}
	for _, s := range sites {
		mu.Lock()
		before := seen
		mu.Unlock()
		s.read()
		mu.Lock()
		after := seen
		mu.Unlock()
		if after == before {
			t.Errorf("%s read %s/traits.memql without the front end of edition 2098, which its domain declares", s.name, domain)
		}
	}
}

// A file its edition's front end refuses is read by no loader, and no loader
// drops it without a word: the walkers with no problem list of their own warn,
// naming it, and dslgate.ScanTree -- an offline entry with an error channel --
// returns an error naming it (review of memql#5358). A warning rather than a
// refusal: these walkers also read a disabled pack's files, which boot does
// not, so a refusal here would let a switched-off pack refuse boot.
func TestAFileItsEditionRefusesIsNamedAtEveryWalker(t *testing.T) {
	unregister := langparser.RegisterEdition(langparser.FrontEnd{
		Edition:        "2094",
		GrammarVersion: "2094.01-refusing",
		Prepare: func(string) (string, error) {
			return "", errors.New("the 2094 front end cannot read this file")
		},
	})
	t.Cleanup(unregister)
	const domain = "editionwarned"
	own := fstest.MapFS{
		domain + "/" + dslfs.ManifestFile: editionLine("2094"),
		domain + "/traits.memql":          {Data: traitSource("trait", "editionWarnedTrait")},
	}

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for _, site := range []struct {
		name string
		read func()
	}{
		{"actions.Registry.LoadFromFS", func() { _, _ = actions.NewRegistry().LoadFromFS(own) }},
		{"actions.LoadCatalogFromFS", func() { _, _ = actions.LoadCatalogFromFS(own) }},
		{"the capability-name loader", func() { _, _ = loadCapabilityNamesFromFS(own) }},
		{"the dependency-tree validator", func() { _ = validateDependencyTree(own) }},
	} {
		buf.Reset()
		site.read()
		if !strings.Contains(buf.String(), domain+"/traits.memql") {
			t.Errorf("%s dropped %s/traits.memql, which its edition's front end refused, without a word; log:\n%s", site.name, domain, buf.String())
		}
	}
	if _, err := dslgate.ScanTree(own, dslgate.Options{}); err == nil || !strings.Contains(err.Error(), domain+"/traits.memql") {
		t.Errorf("dslgate.ScanTree must return an error naming the refused file, got %v", err)
	}
}

// TestAFileItsEditionRefusesIsRefusedAtBoot: a front end may refuse a file.
// That file is then read by no loader -- under the core grammar it could mean
// something else -- and Init refuses the tree naming it, once.
func TestAFileItsEditionRefusesIsRefusedAtBoot(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	unregister := langparser.RegisterEdition(langparser.FrontEnd{
		Edition:        "2097",
		GrammarVersion: "2097.01-refusing",
		Prepare: func(string) (string, error) {
			return "", errors.New("the 2097 front end cannot read this file")
		},
	})
	t.Cleanup(unregister)
	const domain = "editionrefused"
	registerEditionDomain(t, domain, "2097", fstest.MapFS{"traits.memql": {Data: traitSource("trait", "editionRefusedTrait")}})

	for _, f := range baseloader.ReadAll(nil) {
		if f.Path == domain+"/traits.memql" {
			t.Errorf("baseloader.ReadAll returned %s, which its edition's front end refused", f.Path)
		}
	}

	eng := newQuietEngine(t)
	err := eng.Init(loadedConceptRegistry(t))
	if err == nil {
		t.Fatal("Init booted a tree holding a file its edition's front end refused")
	}
	for _, want := range []string{"strict DSL boot refused", domain + "/traits.memql", "the 2097 front end cannot read this file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must carry %q, got:\n%v", want, err)
		}
	}
	refusals := 0
	for _, s := range eng.LoadReport().Skipped {
		if s.Component == "languageLine" && s.File == domain+"/traits.memql" {
			refusals++
		}
	}
	if refusals != 1 {
		t.Errorf("want the refused file on the report exactly once, got %d: %+v", refusals, eng.LoadReport().Skipped)
	}
	if eng.specs.Has("editionRefusedTrait") {
		t.Error("a trait from a file its edition refused was registered")
	}
}
