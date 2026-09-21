package wholesalepack_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

const uniqueDomain = "wholesalepacktest"

func TestWholesalePackLoadsAndExtends(t *testing.T) {
	logger := slog.Default()
	memqldsl.RegisterTree(uniqueDomain, wholesalepack.Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(uniqueDomain) })

	if _, err := memqldsl.Tree().Open(uniqueDomain + "/concepts.memql"); err != nil {
		t.Fatalf("pack concepts.memql not reachable via dsl.Tree(): %v", err)
	}
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts failed: %v", err)
	}
	for _, id := range []string{
		"v1:wholesale:application",
		"v1:wholesale:applicationDecision",
		"v1:wholesale:entitlement",
		"v1:wholesale:wholesaleSettings",
	} {
		if _, err := memorynodes.DefaultRegistry().Get(id); err != nil {
			t.Fatalf("pack concept %q MUST be registered after LoadUnifiedConcepts: %v", id, err)
		}
	}
}

func TestWholesalePackTreeParses(t *testing.T) {
	tree := wholesalepack.Tree()
	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			t.Errorf("%s: read: %v", path, readErr)
			return nil
		}
		rewritten, rewriteErr := parser.NormaliseAll(string(raw))
		if rewriteErr != nil {
			t.Errorf("%s: rewrite: %v", path, rewriteErr)
			return nil
		}
		if _, parseErr := parser.ParseFile(rewritten); parseErr != nil && !errors.Is(parseErr, parser.ErrEmptyInput) {
			t.Errorf("%s: parse: %v", path, parseErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pack tree: %v", err)
	}
}

// THE STOREFRONT PACK CONVENTION, ASSERTED RATHER THAN DESCRIBED.
//
// reviewspack and this pack ship concepts, mutations, queries, shapes,
// tools and builtins and NO automations and NO logic; deploypack and
// referencepack ship both, so this is a choice the storefront packs made
// rather than a rule of the platform (design record, section 7). It is also
// the thing that lets a client lay its own process over the pack: an
// automation here would be a process the client has to work around.
//
// This test is what keeps it from eroding. An `automation` or a `logic`
// construct added to this pack in a hurry would otherwise be found by a
// second client, when their process disagreed with ours.
func TestPackShipsNoAutomationsAndNoLogic(t *testing.T) {
	tree := wholesalepack.Tree()
	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return walkErr
		}
		raw, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			t.Errorf("%s: read: %v", path, readErr)
			return nil
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, banned := range []string{"automation ", "logic "} {
				if strings.HasPrefix(trimmed, banned) {
					t.Errorf("%s:%d declares a %s-- the storefront pack convention is "+
						"concepts, mutations, queries, shapes, tools and builtins and NO "+
						"automations and NO logic, because a process here is one a client "+
						"has to work around: %s", path, i+1, banned, trimmed)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pack tree: %v", err)
	}
}

// NO CLIENT-SPECIFIC FIELD, AND NO BLOB TO PUT ONE IN.
//
// "The first client collects an EIN and the next will not" (design record,
// section 7). A field for it here, or a `metadata` object to hide it in,
// would make the pack's schema drift toward whichever client asked first.
// The words below are the ones a hurried change would actually use.
func TestApplicationCarriesNoClientSpecificField(t *testing.T) {
	raw, err := fs.ReadFile(wholesalepack.Tree(), "concepts.memql")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "@") {
			continue
		}
		for _, banned := range []string{"ein ", "taxId ", "vatNumber ", "metadata ", "customFields ", "extra "} {
			if strings.HasPrefix(trimmed, banned) {
				t.Errorf("concepts.memql:%d declares %q. A client's own field is a RELATED "+
					"CONCEPT in that client's domain with an @relationship to the "+
					"application -- typed and queryable -- never a field or a blob here: %s",
					i+1, strings.TrimSpace(banned), trimmed)
			}
		}
	}
}

func TestWholesalePackContractCompat(t *testing.T) {
	if err := memql.CheckPluginContractCompat(wholesalepack.ContractVersion); err != nil {
		t.Fatal(err)
	}
	reg := memql.PluginRegistration{
		Name:                    wholesalepack.Domain,
		RequiresContractVersion: wholesalepack.ContractVersion,
	}
	if err := reg.ValidateContract(); err != nil {
		t.Fatal(err)
	}
}

// THE PACK SHIPS DISABLED, and the assertion is worth having because the
// default is the difference between an operator choosing to publish a
// public write endpoint and an upgrade choosing it for them.
func TestPackShipsDisabled(t *testing.T) {
	if wholesalepack.DefaultEnabled {
		t.Fatal("the wholesale pack must ship DISABLED: enabling it publishes a write " +
			"endpoint on every deployable whose shopperForms is on, and what that endpoint " +
			"collects is a named person's business contact details")
	}
}
