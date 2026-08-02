package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file resolves a construct's signature-bound concept SHORT NAME
// (e.g. `space` in `query space spaceParticipants`) to its canonical
// concept id (e.g. `v1:cognition:space`), mirroring the engine's
// component/memql.LoadUnifiedConcepts resolution model:
//
//   - A directory-keyed index maps <namespace-dir> -> <concept short name>
//     -> canonical id, assembled from each concept's @version + @namespace
//     attributes + name (component/language/ast.AssembleConceptId).
//   - A construct's short name resolves same-namespace-first (its own
//     directory), then through the file-top `use <dir>.concepts.{ name }`
//     imports (cross-namespace), exactly like resolveRelationshipTargetName.
//
// It is deliberately a light, stdlib-only regex parser (the whole sdk/gen
// package is, so a product BFF can import it cheaply) rather than pulling in
// the engine's concept loader. The id-assembly + import model are simple and
// pinned by the engine; the resolution tests guard against drift.

var (
	// conceptHeaderRe matches a top-level `concept <name> {` declaration.
	conceptHeaderRe = regexp.MustCompile(`(?m)^concept[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	// versionAttrRe / namespaceAttrRe pull the string literal from the
	// @version / @namespace annotation in a concept's attribute preamble.
	versionAttrRe   = regexp.MustCompile(`@version\("([^"]*)"\)`)
	namespaceAttrRe = regexp.MustCompile(`@namespace\("([^"]*)"\)`)
	// useConceptsRe matches a file-top `use <dir>.concepts.{ a, b }` import.
	// Only `.concepts.` imports participate in concept resolution.
	useConceptsRe = regexp.MustCompile(`(?m)^use[ \t]+([A-Za-z_][A-Za-z0-9_]*)\.concepts\.\{([^}]*)\}`)
	// patternAnnotationRe extracts the regex source from an args-field
	// `@pattern("...")` annotation.
	patternAnnotationRe = regexp.MustCompile(`@pattern\("([^"]*)"\)`)
)

// conceptIndex maps a namespace directory -> concept short name -> canonical id.
type conceptIndex map[string]map[string]string

// buildConceptIndex walks every root, parses each concept declaration, and
// records its canonical id under the namespace DIRECTORY the file lives in
// (the segment a `use <dir>.concepts` import names). Roots are merged; a
// concept present in more than one root under the same (dir, name) resolves
// to the same content-derived id, so last-writer is harmless.
func buildConceptIndex(roots []string) conceptIndex {
	idx := conceptIndex{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Skip authoring skeletons (dsl/_reference/), exactly as the
				// engine walker + CollectConstructs do.
				if d.Name() != "." && strings.HasPrefix(d.Name(), "_") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".memql") {
				return nil
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil // best-effort; a missing/unreadable file just yields no concepts
			}
			src := string(raw)
			dir := dirForPath(root, path)
			for _, m := range conceptHeaderRe.FindAllStringSubmatchIndex(src, -1) {
				name := src[m[2]:m[3]]
				id := assembleConceptIdFromPreamble(src, m[0], name, dir)
				if id == "" {
					continue // concept missing @version/@namespace -- not addressable
				}
				if idx[dir] == nil {
					idx[dir] = map[string]string{}
				}
				idx[dir][name] = id
			}
			return nil
		})
	}
	return idx
}

// assembleConceptIdFromPreamble reads the @version + @namespace annotations
// from the attribute block immediately preceding a concept header and returns
// the canonical id `v<major>:<namespace>:<name>`. Absent @version defaults to
// major 1 (#2613) and absent @namespace derives the containing domain
// directory (#2614) -- both mirroring AssembleConceptIdFromDeclInDir in
// LOCKSTEP (the PR #2657 law: the two assemblers change together).
func assembleConceptIdFromPreamble(src string, headerStart int, name, dir string) string {
	preamble := attrPreamble(src, headerStart)
	vm := versionAttrRe.FindStringSubmatch(preamble)
	nm := namespaceAttrRe.FindStringSubmatch(preamble)
	major := 1
	if vm != nil {
		major = majorVersion(vm[1])
	}
	namespace := dir
	if nm != nil {
		namespace = strings.TrimSpace(nm[1])
	}
	if major < 0 || namespace == "" || name == "" {
		return ""
	}
	return "v" + strconv.Itoa(major) + ":" + namespace + ":" + name
}

// majorVersion parses the MAJOR component out of a "MAJOR.MINOR.PATCH" string.
// Returns -1 on a malformed version.
func majorVersion(semver string) int {
	head := strings.SplitN(strings.TrimSpace(semver), ".", 2)[0]
	n, err := strconv.Atoi(head)
	if err != nil {
		return -1
	}
	return n
}

// parseUseImports returns the concept short-name -> namespace-directory map
// declared by a file's `use <dir>.concepts.{ ... }` imports.
func parseUseImports(src string) map[string]string {
	out := map[string]string{}
	for _, m := range useConceptsRe.FindAllStringSubmatch(src, -1) {
		dir := m[1]
		for _, raw := range strings.Split(m[2], ",") {
			name := strings.TrimSpace(raw)
			if name != "" {
				out[name] = dir
			}
		}
	}
	return out
}

// resolveConceptId resolves a construct's bound short name to its canonical id:
// same-namespace (the construct's own directory) first, then through the file's
// `use` imports. Returns "" when unresolved (an out-of-tree fixture or a concept
// that hasn't migrated to @version/@namespace) so callers degrade gracefully.
func resolveConceptId(short, dir string, imports map[string]string, idx conceptIndex) string {
	if short == "" {
		return ""
	}
	if m, ok := idx[dir]; ok {
		if id, ok := m[short]; ok {
			return id
		}
	}
	if importedDir, ok := imports[short]; ok {
		if m, ok := idx[importedDir]; ok {
			if id, ok := m[short]; ok {
				return id
			}
		}
	}
	return ""
}

// canonicalArgPatternExemptions lists `<construct>.<argField>` keys whose
// `@pattern("^v1:...")` is a KNOWN, tracked deferral rather than a
// regression -- kept in lockstep with the A1 conformance exemption
// (dsl/conformance_test.go: TestNoCanonicalPatternOnArgs). Prune an entry
// when the underlying pattern is removed so the gate covers it again.
var canonicalArgPatternExemptions = map[string]bool{
	// spaceMedia.partitionId carries a v1:cognition:space id; `space` is
	// pack-owned (not in the engine tree) so the engine can't declare an
	// @relationship to canonicalize a bare arg, and a bare-only pattern would
	// reject the canonical ids the current SPA still sends. Rides A2's
	// partition_id->space_id rename (#2441) + the A4 SPA cutover (#2443).
	"spaceMedia.partitionId": true,
}

// assertNoCanonicalArgPatterns fails generation when any collected arg carries
// a `@pattern` anchored on the canonical id form (`^v1:` / `v1:`). Such a
// pattern forces the caller to compose a canonical id -- exactly the coupling
// the bare-ids client contract (#2438) removes. Belt-and-suspenders with the
// A1 DSL conformance rule: even if a canonical pattern reaches sdk-gen, no SDK
// that teaches the old contract is emitted.
func assertNoCanonicalArgPatterns(constructs []Construct) error {
	var offenders []string
	for _, c := range constructs {
		for _, a := range c.Args {
			p := strings.TrimSpace(a.Pattern)
			if p == "" {
				continue
			}
			if !strings.HasPrefix(p, "^v1:") && !strings.HasPrefix(p, "v1:") {
				continue
			}
			if canonicalArgPatternExemptions[c.Name+"."+a.Name] {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s.%s (@pattern=%q) in %s", c.Name, a.Name, p, c.Origin))
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	sort.Strings(offenders)
	return fmt.Errorf(
		"sdk-gen: %d client-facing arg(s) carry a canonical-id @pattern -- clients must pass BARE ids (#2438); drop the pattern or relax it to a bare-slug form:\n  %s",
		len(offenders), strings.Join(offenders, "\n  "))
}

// conceptRegistry flattens the concept index into the sorted, de-duplicated
// list of every canonical concept id discovered across the roots. This is the
// authoritative concept set the generated concept/topic constants enumerate.
func conceptRegistry(idx conceptIndex) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range idx {
		for _, id := range m {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

// dirForPath returns the first path segment of `path` relative to `root` --
// the namespace directory used as the concept-index key and matched by a
// `use <dir>.concepts` import's leading segment. Falls back to the parent
// directory's base name when the relative computation fails.
func dirForPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(filepath.Dir(path))
	}
	rel = filepath.ToSlash(rel)
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	// File directly under root (no namespace directory).
	return ""
}
