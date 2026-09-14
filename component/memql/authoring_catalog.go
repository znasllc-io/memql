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
	s = neutralizeLambdaParams(s)
	if name != "" {
		nameRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		s = nameRe.ReplaceAllString(s, "_")
	}
	s = catalogWsRe.ReplaceAllString(s, " ")
	s = catalogPunctRe.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

// catalogLambdaHeaderRe matches an edition-2026 lambda header: `x =>` or
// `(a, b) =>`.
var catalogLambdaHeaderRe = regexp.MustCompile(`(?:\(([^()]*)\)|\b([A-Za-z_][A-Za-z0-9_]*))\s*=>`)

// neutralizeLambdaParams renames every lambda parameter to a positional
// placeholder (`_p0`, `_p1`, ... in order of first appearance), so two
// constructs that differ only in what they called the row -- `row => row.a ==
// 1` and `r => r.a == 1` -- share a key (epic memql#5363). A legacy predicate
// named no parameter at all, so without this every edition-2026 construct
// would carry a degree of freedom the dedup layer reads as a difference, and
// the planner would author a duplicate of a construct it could have reused.
//
// A member name is never a parameter (`row.row` renames only the root), and
// string contents are left alone.
func neutralizeLambdaParams(s string) string {
	code := catalogStructureOf(s)
	var params []string
	seen := map[string]bool{}
	for _, m := range catalogLambdaHeaderRe.FindAllStringSubmatch(code, -1) {
		names := []string{m[2]}
		if m[2] == "" {
			names = strings.Split(m[1], ",")
		}
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" && !seen[n] {
				seen[n] = true
				params = append(params, n)
			}
		}
	}
	if len(params) == 0 {
		return s
	}
	rename := make(map[string]string, len(params))
	for i, p := range params {
		rename[p] = fmt.Sprintf("_p%d", i)
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if code[i] == ' ' && s[i] != ' ' {
			b.WriteByte(s[i]) // inside a string literal: kept verbatim
			i++
			continue
		}
		if isCatalogIdentStart(s[i]) && (i == 0 || (!isCatalogIdentByte(s[i-1]) && s[i-1] != '.')) {
			j := i
			for j < len(s) && isCatalogIdentByte(s[j]) {
				j++
			}
			if r, ok := rename[s[i:j]]; ok && code[i] != ' ' {
				b.WriteString(r)
			} else {
				b.WriteString(s[i:j])
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// catalogStructureOf returns s with double-quoted string contents
// blanked to spaces, byte for byte, so a scan for headers and identifiers
// never reads a quoted word as code.
func catalogStructureOf(s string) string {
	out := []byte(s)
	inStr := false
	for i := 0; i < len(out); i++ {
		switch {
		case inStr && out[i] == '\\' && i+1 < len(out):
			out[i], out[i+1] = ' ', ' '
			i++
		case out[i] == '"':
			inStr = !inStr
		case inStr:
			out[i] = ' '
		}
	}
	return string(out)
}

func isCatalogIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isCatalogIdentByte(c byte) bool {
	return isCatalogIdentStart(c) || c >= '0' && c <= '9'
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
