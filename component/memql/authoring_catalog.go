package memql

// authoring_catalog.go -- the dedup/match foundation for the per-owner
// reusable catalog (epic memql#954, issue #957).
//
// "Compose-first, author-the-gap" needs to recognize when a construct the
// planner is about to author is functionally the same as one that already
// exists, so it reuses instead of duplicating. This file computes a stable,
// NAME-INDEPENDENT signature of a construct (CatalogKey) and an exact
// structural match against a candidate set. Two constructs of the same kind
// whose source is identical modulo their own name (and comments / whitespace)
// produce the same CatalogKey.
//
// This is the deterministic layer. The fuzzy "find a CLOSE-but-not-identical
// reusable construct" path (embed cataloged constructs, similarTo retrieval)
// is the next #957 increment and layers on top of this: an exact CatalogKey
// hit is a safe direct reuse; a near miss is a similarTo candidate.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

var (
	catalogCommentRe = regexp.MustCompile(`(?m)//.*$`)
	catalogWsRe      = regexp.MustCompile(`\s+`)
	// catalogPunctRe drops whitespace adjacent to any punctuation / operator
	// char so operator spacing (`a == b` vs `a==b`) doesn't change the key.
	// Whitespace between two word tokens (e.g. `spec _`) is preserved.
	catalogPunctRe = regexp.MustCompile(`\s*([^\w\s])\s*`)
)

// CatalogKey computes a stable, name-independent signature of a construct's
// behavior. Same kind + same source (modulo the construct's own name,
// comments, and whitespace) -> same key. Returns an error if the source does
// not parse for the given kind (you should only catalog constructs that
// compiled in the sandbox).
//
// NOTE: kinds this layer handles mirror the sandbox compile pass --
// query / mutation / logic / spec / trait / shape. Other kinds return an
// error (follow-up, alongside the sandbox's automation/concept support).
func CatalogKey(kind, source string) (string, error) {
	name, err := constructName(kind, source)
	if err != nil {
		return "", err
	}
	canon := canonicalizeConstructSource(source, name)
	sum := sha256.Sum256([]byte(kind + "\x00" + canon))
	return kind + ":" + hex.EncodeToString(sum[:]), nil
}

// CatalogEntry is a cataloged construct's identity for matching.
type CatalogEntry struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	CatalogKey string `json:"catalogKey"`
}

// FindCatalogMatch returns the cataloged entry whose CatalogKey EXACTLY matches
// the candidate (a structural match modulo name -> safe to reuse directly), or
// nil if none. Only entries of the candidate's kind are considered. Returns an
// error if the candidate source does not parse.
func FindCatalogMatch(candidateKind, candidateSource string, catalog []CatalogEntry) (*CatalogEntry, error) {
	key, err := CatalogKey(candidateKind, candidateSource)
	if err != nil {
		return nil, err
	}
	for i := range catalog {
		if catalog[i].Kind == candidateKind && catalog[i].CatalogKey == key {
			return &catalog[i], nil
		}
	}
	return nil, nil
}

// canonicalizeConstructSource strips line comments, neutralizes the construct's
// own name (whole-word -> "_"), and collapses whitespace, so two constructs
// that differ only by name / formatting hash identically.
func canonicalizeConstructSource(source, name string) string {
	s := catalogCommentRe.ReplaceAllString(source, "")
	if name != "" {
		nameRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		s = nameRe.ReplaceAllString(s, "_")
	}
	s = catalogWsRe.ReplaceAllString(s, " ")
	s = catalogPunctRe.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

// constructName parses a single construct's source and returns its declared
// name, routing to the same per-construct parsers the sandbox uses.
func constructName(kind, source string) (string, error) {
	switch kind {
	case "query", "mutation", "logic":
		slices := ExtractFunctionSlices(source)
		if len(slices) == 0 {
			return "", fmt.Errorf("catalog: no %s declaration found in source", kind)
		}
		return slices[0].Name, nil
	case "spec", "trait":
		decl, err := languageParser.ParseSpecDecl(source)
		if err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return decl.Name, nil
	case "shape":
		decl, err := languageParser.ParseShapeDecl(source)
		if err != nil {
			return "", fmt.Errorf("catalog: %w", err)
		}
		return decl.Name, nil
	default:
		return "", fmt.Errorf("catalog: kind %q is not yet supported (follow-up #957)", kind)
	}
}
