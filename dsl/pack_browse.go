package dsl

import (
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Pack-browsing surface (memql#2127 / B1)
// =======================================
//
// A read-only enumerator over the embedded + plugin-registered .memql
// tree, plus the MEMQL_DSL_PATH on-disk overlay. It powers the cockpit's
// "browse packs" view: list domains, list the .memql files inside a
// domain, fetch a single file's source. There is NO write path here --
// the embedded tree is immutable and a pack subtree is owned by its
// registering module; this package only reads.
//
// Origin labels
// -------------
//   - "embedded"     -- the file/domain comes from this binary's baked-in
//                       //go:embed tree (dsl/<domain>/...).
//   - "pack:<domain>"-- the domain was contributed at init() time via
//                       RegisterTree by an external Go module (a "pack").
//   - "disk:<path>"  -- the file is being served from an on-disk override
//                       under MEMQL_DSL_PATH instead of the embedded copy
//                       (see component/memql/dslfs). Only ReadPackFile
//                       reports this, since the overlay is resolved at
//                       per-file read time.
//
// The functions take an origin-label picker as an injected dependency so
// this package stays free of the component/memql import cycle
// (component/memql depends on dsl, not the reverse). The gRPC handler passes
// the dslfs origin helper in.

// PackDomain is one browsable namespace in the unified DSL tree.
type PackDomain struct {
	// Name is the top-level namespace directory (e.g. "cognition", or
	// a pack-registered domain name).
	Name string
	// Origin is "embedded" for a core baked-in domain or "pack:<name>"
	// for a RegisterTree'd plugin domain.
	Origin string
	// FileCount is the number of .memql files directly visible under the
	// domain (recursive). Convenience for the list view so the cockpit
	// doesn't have to round-trip ListPackFiles just to show a count.
	FileCount int
}

// PackFile is one .memql source file inside a domain.
type PackFile struct {
	// Path is the file path RELATIVE to the domain root, slash-separated
	// (e.g. "queries.memql", "prompts/agentReply.tmpl"). Combined with
	// the domain name it re-roots into the unified tree as
	// "<domain>/<path>".
	Path string
	// Size is the file's byte length.
	Size int64
}

// listMemqlSuffixes are the file extensions the pack browser surfaces.
// .memql is the DSL source; .tmpl files are prompt templates that live
// alongside prompts.memql and are part of the authored pack surface.
var listMemqlSuffixes = []string{".memql", ".tmpl"}

func hasBrowsableSuffix(name string) bool {
	for _, s := range listMemqlSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// ListPackDomains returns every browsable domain in the unified tree --
// core embedded domains plus any RegisterTree'd pack domains -- sorted by
// name. The "_"-prefixed authoring-skeleton directories (e.g.
// "_reference") are excluded; they are not live namespaces. FileCount is
// the recursive count of browsable files under each domain.
func ListPackDomains() []PackDomain {
	tree := Tree()
	entries, err := fs.ReadDir(tree, ".")
	if err != nil {
		return nil
	}

	// The set of core (embedded) domain names, so we can label each
	// top-level entry as embedded vs pack without re-reading the FS per
	// entry.
	core := map[string]struct{}{}
	for _, d := range coreDomains() {
		core[d] = struct{}{}
	}

	out := make([]PackDomain, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") {
			continue
		}
		origin := "embedded"
		if _, ok := core[name]; !ok {
			origin = "pack:" + name
		}
		out = append(out, PackDomain{
			Name:      name,
			Origin:    origin,
			FileCount: countBrowsableFiles(tree, name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// countBrowsableFiles walks domain/ in tree and counts browsable files.
func countBrowsableFiles(tree fs.FS, domain string) int {
	n := 0
	_ = fs.WalkDir(tree, domain, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if hasBrowsableSuffix(d.Name()) {
			n++
		}
		return nil
	})
	return n
}

// ListPackFiles returns the browsable files under a single domain, sorted
// by relative path. domain must be a top-level namespace returned by
// ListPackDomains; an unknown domain yields an empty slice (not an error
// -- the caller distinguishes "no such domain" via ListPackDomains).
func ListPackFiles(domain string) []PackFile {
	domain = strings.TrimSpace(domain)
	if domain == "" || strings.Contains(domain, "/") || strings.HasPrefix(domain, "_") {
		return nil
	}
	tree := Tree()
	// Confirm the domain exists before walking so a bogus name returns
	// empty rather than letting WalkDir surface a partial.
	if info, err := fs.Stat(tree, domain); err != nil || !info.IsDir() {
		return nil
	}

	out := []PackFile{}
	_ = fs.WalkDir(tree, domain, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !hasBrowsableSuffix(d.Name()) {
			return nil
		}
		rel := strings.TrimPrefix(p, domain+"/")
		var size int64
		if fi, ferr := d.Info(); ferr == nil {
			size = fi.Size()
		}
		out = append(out, PackFile{Path: rel, Size: size})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ReadPackFile returns the source of a single file inside a domain. The
// relPath is relative to the domain root (as returned by ListPackFiles);
// it is cleaned and validated to stay within the domain (no "..", no
// absolute escape). Returns the file source and an origin label.
//
// origin is "embedded" or "pack:<domain>" matching ListPackDomains. The
// MEMQL_DSL_PATH disk-overlay label is NOT resolved here -- the overlay
// is per-construct-type, indirected through component/memql/dslfs, and
// the gRPC handler layers it in (see PackFileOrigin). Keeping this
// function dependency-free preserves the dsl-package import direction.
//
// A missing file or an out-of-domain path yields (nil, "", err).
func ReadPackFile(domain, relPath string) (source []byte, origin string, err error) {
	domain = strings.TrimSpace(domain)
	relPath = strings.TrimSpace(relPath)

	full, ok := packFilePath(domain, relPath)
	if !ok {
		return nil, "", fs.ErrNotExist
	}

	tree := Tree()
	data, err := fs.ReadFile(tree, full)
	if err != nil {
		return nil, "", err
	}

	origin = "embedded"
	if _, isCore := coreDomainSet()[domain]; !isCore {
		origin = "pack:" + domain
	}
	return data, origin, nil
}

// PackFileExists reports whether domain/relPath resolves to a browsable
// file in the unified tree. Useful for the handler's validation step
// before it consults the overlay.
func PackFileExists(domain, relPath string) bool {
	full, ok := packFilePath(strings.TrimSpace(domain), strings.TrimSpace(relPath))
	if !ok {
		return false
	}
	info, err := fs.Stat(Tree(), full)
	return err == nil && !info.IsDir()
}

// packFilePath validates domain + relPath and returns the cleaned
// unified-tree path "<domain>/<relPath>". ok is false for an empty /
// slash-containing / "_"-prefixed domain, a non-browsable suffix, or a
// relPath that escapes the domain root.
func packFilePath(domain, relPath string) (string, bool) {
	if domain == "" || strings.Contains(domain, "/") || strings.HasPrefix(domain, "_") {
		return "", false
	}
	if relPath == "" || !hasBrowsableSuffix(relPath) {
		return "", false
	}
	cleaned := path.Clean(relPath)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return domain + "/" + cleaned, true
}

// coreDomainSet returns the core embedded domains as a set for O(1)
// origin lookups.
func coreDomainSet() map[string]struct{} {
	set := map[string]struct{}{}
	for _, d := range coreDomains() {
		set[d] = struct{}{}
	}
	return set
}
