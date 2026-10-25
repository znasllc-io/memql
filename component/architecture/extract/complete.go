package extract

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// requireCompleteLoad checks the entire dependency closure, before exclusions
// select the nodes to render. An excluded broken dependency still invalidates
// type information for its callers. Edge multiplicities are unrelated to load
// completeness and are deliberately preserved.
func requireCompleteLoad(roots []*packages.Package, typed bool) error {
	if len(roots) == 0 {
		return fmt.Errorf("no packages matched the declared roots")
	}
	var failures []string
	packages.Visit(roots, nil, func(p *packages.Package) {
		for _, err := range p.Errors {
			failures = append(failures, p.PkgPath+": "+err.Error())
		}
		if typed && p.IllTyped && len(p.Errors) == 0 {
			failures = append(failures, p.PkgPath+": incomplete type information")
		}
		// unsafe has synthetic type information and no Go source. Every actual
		// compiled source file must participate in the requested type/syntax pass.
		if typed && p.PkgPath != "unsafe" && len(p.CompiledGoFiles) > 0 && (p.Types == nil || p.TypesInfo == nil || len(p.Syntax) != len(p.CompiledGoFiles)) {
			failures = append(failures, p.PkgPath+": missing type or syntax data")
		}
	})
	if len(failures) == 0 {
		return nil
	}
	sort.Strings(failures)
	total := len(failures)
	if total > 8 {
		failures = failures[:8]
	}
	return fmt.Errorf("%d package load problem(s): %s", total, strings.Join(failures, "; "))
}
