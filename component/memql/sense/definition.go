package sense

// definition.go implements go-to-definition (memql#2754): given a cursor on a
// construct reference, answer where that construct is declared.
//
// It matters most for the ambient case. Same-domain constructs are referenced
// with no `use` import (authoring rule 25, #2617), so `candidate` in `shape
// candidate candidateFull` has no import line pointing at its source -- the
// editor is the only way to find it.

import "strings"

// DefinitionResult is one resolved definition target: the workspace-relative
// file declaring the construct, its kind, and the range of the declared name
// token within that file.
type DefinitionResult struct {
	File  string
	Kind  string
	Range Range
}

// Definition resolves the construct referenced at (line, col) to its
// declaration site(s). filePath is the referencing document's path; its
// directory supplies the ambient domain used to break a cross-namespace tie.
//
// It returns nothing rather than guessing. A name that resolves to several
// declarations and cannot be narrowed by the referencing file's own domain is
// left unresolved: sending the editor to the wrong file is worse than not
// jumping, and matches the conservatism the rest of the workspace seam uses.
func (s *Service) Definition(source string, line, col int, filePath string) []DefinitionResult {
	if source == "" || s.workspace == nil {
		return nil
	}
	token, _ := tokenAtPosition(source, line, col)
	if token == "" {
		return nil
	}
	// A canonical id (`v1:actions:candidate`) lexes as one token; the
	// declaration is keyed by its trailing segment.
	name := token
	if idx := strings.LastIndex(name, ":"); idx >= 0 {
		name = name[idx+1:]
	}
	// A dotted path (`row.id`, `args.limit`) also lexes as one token and
	// never names a construct.
	if name == "" || strings.Contains(name, ".") {
		return nil
	}

	sites := s.workspace.DeclarationSites(name)
	if len(sites) == 0 {
		return nil
	}
	if len(sites) > 1 {
		sites = narrowByDomain(sites, fileDomain(filePath))
		if len(sites) != 1 {
			return nil
		}
	}

	out := make([]DefinitionResult, 0, len(sites))
	for _, site := range sites {
		out = append(out, DefinitionResult{
			File: site.File,
			Kind: site.Kind,
			Range: Range{
				Start: Position{Line: site.Line, Column: site.Column},
				End:   Position{Line: site.Line, Column: site.Column + len(site.Name)},
			},
		})
	}
	return out
}

// narrowByDomain keeps only the sites declared in `domain`. An empty domain
// narrows nothing, which leaves the caller with an ambiguous set and therefore
// no jump.
func narrowByDomain(sites []DeclSite, domain string) []DeclSite {
	if domain == "" {
		return sites
	}
	var kept []DeclSite
	for _, site := range sites {
		if fileDomain(site.File) == domain {
			kept = append(kept, site)
		}
	}
	if len(kept) == 0 {
		return sites
	}
	return kept
}
