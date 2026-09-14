package memql

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// PolicyConfig is a loaded policy: its @primary (one) and @fallback (many)
// entries. Used by Router to resolve a policy name to an ordered chain of
// entries.
//
// MaxLatencyMs, MaxTimeToFirstTokenMs and PreferredRoles were removed with
// epic memql#5127. All three were parsed, stored, projected into the catalog
// -- and read by no selection path at all. An annotation that reads as
// configuration while steering nothing is worse than its absence.
type PolicyConfig struct {
	Name        string
	Description string
	Primary     string
	Fallbacks   []string
}

// ProviderChain returns primary followed by fallbacks, which is the
// order the Router attempts providers in.
func (p PolicyConfig) ProviderChain() []string {
	chain := make([]string, 0, len(p.Fallbacks)+1)
	if strings.TrimSpace(p.Primary) != "" {
		chain = append(chain, p.Primary)
	}
	for _, f := range p.Fallbacks {
		f = strings.TrimSpace(f)
		if f != "" {
			chain = append(chain, f)
		}
	}
	return chain
}

// policyDeclToPolicyConfig translates the langparser-produced AST
// node into the *PolicyConfig the unified policy loader stores in
// PolicyRegistry: the policy name + @primary are required; the
// @fallback list is passed through in declaration order. (The
// hand-rolled parser that applied the same rules at the end of its own
// parse is deleted -- it was reached only by its test, memql#5359.)
//
// The loader (LoadUnifiedPolicies) overrides Name with the slice
// name from `ExtractKeywordSlices` to keep parity with the legacy
// behaviour where the slice's bracketed name wins over whatever
// the parser captured from `policy NAME { ... }` (matters when
// declaration name differs from the on-disk slice filename for
// historical-rename reasons).
func policyDeclToPolicyConfig(decl *ast.PolicyDecl) (*PolicyConfig, error) {
	if decl == nil {
		return nil, fmt.Errorf("policy declaration is nil")
	}
	if decl.Name == "" {
		return nil, fmt.Errorf("policy: name is required")
	}
	if decl.Primary == "" {
		return nil, fmt.Errorf("policy %q: @primary is required", decl.Name)
	}
	return &PolicyConfig{
		Name:        decl.Name,
		Description: languageParser.EffectiveDescription(decl.DocComment, decl.Description),
		Primary:     decl.Primary,
		Fallbacks:   decl.Fallbacks,
	}, nil
}
