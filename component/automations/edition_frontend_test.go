package automations_test

// edition_frontend_test.go -- the automations loader walks the tree itself
// rather than reading through baseloader.ReadAll, so it is its own parse site
// for memql#5358: every automations.memql reaches the compiler only through the
// front end of the edition its domain declares.

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/automations"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// reactionHeader is the spelling only the synthetic edition 2096 knows:
// `reaction NAME {` for what 2026 writes as `automation NAME {`.
var reactionHeader = regexp.MustCompile(`(?m)^([ \t]*)reaction([ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{)`)

func TestAutomationsAreReadThroughTheirDomainsEdition(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "")
	unregister := languageParser.RegisterEdition(languageParser.FrontEnd{
		Edition:        "2096",
		GrammarVersion: "2096.01-synthetic-reaction",
		Prepare: func(src string) (string, error) {
			return reactionHeader.ReplaceAllString(src, "${1}automation${2}"), nil
		},
	})
	t.Cleanup(unregister)

	src := strings.Replace(liveAutomation, "automation blockCommentControl {", "reaction editionReaction {", 1)
	if src == liveAutomation {
		t.Fatal("the fixture did not take the 2096 spelling; this test would examine nothing")
	}

	load := func(t *testing.T, domain, edition string) []string {
		t.Helper()
		line := dslfs.Manifest{Language: languageParser.LanguageVersion, Edition: edition}
		memqldsl.RegisterTree(domain, fstest.MapFS{
			dslfs.ManifestFile:  {Data: []byte(line.Render())},
			"automations.memql": {Data: []byte(src)},
		})
		t.Cleanup(func() { memqldsl.UnregisterTree(domain) })
		loaded, err := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)}).LoadAll()
		if err != nil {
			t.Fatalf("LoadAll: %v", err)
		}
		names := make([]string, 0, len(loaded))
		for _, a := range loaded {
			names = append(names, a.Name)
		}
		return names
	}

	t.Run("a domain declaring 2096 reads the 2096 spelling", func(t *testing.T) {
		if names := load(t, "automationedition", "2096"); !contains(names, "editionReaction") {
			t.Error("the automation written in edition 2096 did not load; the loader read it around its domain's front end")
		}
	})
	t.Run("the same file in a domain declaring 2026 declares no automation", func(t *testing.T) {
		if names := load(t, "automationedition2026", "2026"); contains(names, "editionReaction") {
			t.Error("a 2026 domain's `reaction` block loaded as an automation; only edition 2096 reads that spelling")
		}
	})
}
