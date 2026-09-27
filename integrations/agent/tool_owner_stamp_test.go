package agent

import (
	"sort"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// A tool argument named ownerUserId says whose behalf the call acts on, and a
// model must never be the one to say it. Every tool that declares the field
// declares it @autoInjected -- applyToolDefaults drops the model's value and
// the runtime's stamp (agentContextStamps) puts the turn's owner back -- so the
// owner a tool acts for is the turn's, whatever the model wrote.
//
// ensureAgent was the one tool that took it from the model (memql#5436
// review). The coverage half -- that an @autoInjected field has a stamp at all
// -- is TestAgentToolDefaultsCoverEveryAutoInjectedField; this is the half that
// says the field must be @autoInjected in the first place.
func TestEveryToolOwnerFieldIsServerStamped(t *testing.T) {
	tree, err := dslimports.Load(memqldsl.Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v -- the DSL tree did not parse, so no tool was checked", err)
	}
	var checked, unstamped []string
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			decl, ok := def.(*languageAst.ToolDecl)
			if !ok || decl.Disabled {
				continue
			}
			for _, f := range decl.Fields {
				if f.Name != "ownerUserId" {
					continue
				}
				checked = append(checked, decl.Name)
				if !f.AutoInjected {
					unstamped = append(unstamped, decl.Name)
				}
			}
		}
	}
	sort.Strings(unstamped)
	for _, name := range unstamped {
		t.Errorf("tool %q declares ownerUserId without @autoInjected, so the model chooses whose "+
			"behalf the call acts on. Mark it @autoInjected and give the tool an agentContextStamps "+
			"entry with StampOwnerUserId, as produceArtifact and requestUserFeedback do.", name)
	}
	// A floor, so a parse that stops reaching the tools cannot pass this
	// vacuously: three tools declare the field today.
	if len(checked) < 3 {
		t.Fatalf("found %d tools declaring ownerUserId (%v); three were measured -- the walk has "+
			"stopped reaching them, and this gate now checks nothing", len(checked), checked)
	}
}
