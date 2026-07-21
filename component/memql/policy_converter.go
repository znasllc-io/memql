package memql

import (
	"fmt"
	languageParser "github.com/znasllc-io/memql/component/language/parser"

	"github.com/znasllc-io/memql/component/language/ast"
)

// policyDeclToPolicyConfig translates the langparser-produced AST
// node into the *PolicyConfig the unified policy loader stores in
// PolicyRegistry. Mirrors the validation rules the legacy
// `parsePolicyMemQL` applied at the end of its parse: the policy
// name + @primary are required; @fallback / @preferredRole lists
// are passed through in declaration order; tuning knobs default
// to zero.
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
		Name:                  decl.Name,
		Description:           languageParser.EffectiveDescription(decl.DocComment, decl.Description),
		Primary:               decl.Primary,
		Fallbacks:             decl.Fallbacks,
		MaxLatencyMs:          decl.MaxLatencyMs,
		MaxTimeToFirstTokenMs: decl.MaxTimeToFirstTokenMs,
		PreferredRoles:        decl.PreferredRoles,
	}, nil
}
