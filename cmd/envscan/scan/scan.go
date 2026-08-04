// Package scan is the reusable core of the env-var classifier for Epic 7
// (memql#2103). It is the single source of truth for "which env vars does
// the code read", shared by the envscan command (cmd/envscan) and by the
// CI drift gate (the TestNoEnvRegistryDrift Go test, 7.2 / #2105) so the
// two can never diverge.
//
// It detects two drift directions against the genesis manifest registry
// (scripts/secrets/manifest.yaml, the locked source of truth):
//
//   - forward drift  -- a var read by code that is NOT registered.
//   - reverse drift   -- a registered var that appears NOWHERE in the
//     repo (code / k8s / .env / dsl). Stale registry entry.
//
// Reads are detected from direct, statically-resolvable access sites
// only (os.Getenv / os.LookupEnv, the component/config env* helpers, and
// DSL env("...") literals). Indirection through env.NewEnvReader
// prefixes resolves to a full uppercase key elsewhere in the tree, so
// those keys are still caught by the reverse-drift repo scan -- they are
// just not *forced* by the forward check.
package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/genesis"
)

// external is the explicit allow-list of env keys that are NOT
// memQL-owned configuration: CI / build / OS / runtime-platform vars.
// They are excluded from BOTH drift directions (the epic calls out
// GITHUB_STEP_SUMMARY, K_REVISION, VERSION specifically). Keep this
// tight -- anything memQL actually configures belongs in the registry.
var external = map[string]bool{
	"GITHUB_STEP_SUMMARY": true,
	"K_REVISION":          true,
	"VERSION":             true,
	"TEST_BOOL":           true, // env-reader unit-test sentinel
	"HOME":                true,
	"PATH":                true,
	"PWD":                 true,
	"USER":                true,
	"TMPDIR":              true,
	"TERM":                true,
	"LANG":                true,
	"TZ":                  true,
	"CI":                  true,
	// LiveKit runtime-platform var (LiveKit's own convention, not memQL-owned
	// config): the externally-dialable LiveKit URL that avatardirect + the
	// voice-agent hand to the cloud media plane. The memQL-owned equivalent is
	// MEMQL_POLYPHON_LIVEKIT_PUBLIC_URL; this bare form is the third-party knob.
	"LIVEKIT_PUBLIC_URL": true,
}

// externalPrefixes are key prefixes owned by the CI / build / Go
// toolchain rather than memQL.
var externalPrefixes = []string{"GITHUB_", "RUNNER_", "GO"}

func isExternal(key string) bool {
	if external[key] {
		return true
	}
	for _, p := range externalPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// goReadPatterns match a direct, statically-resolvable env read in Go
// source. The first capture group is the env key.
var goReadPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bos\.Getenv\(\s*"([A-Z][A-Z0-9_]+)"`),
	regexp.MustCompile(`\bos\.LookupEnv\(\s*"([A-Z][A-Z0-9_]+)"`),
	// component/config env* helpers: envStr / envBool / envInt /
	// envFloat / envStrDefault / ...
	regexp.MustCompile(`\benv[A-Z][A-Za-z0-9]*\(\s*"([A-Z][A-Z0-9_]+)"`),
}

// memqlReadPatterns match a DSL provider-auth read. The bare env("...")
// form is DSL-only -- in Go it appears solely in doc-comments, so it is
// scoped to .memql files to avoid false positives.
var memqlReadPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\benv\(\s*"([A-Z][A-Z0-9_]+)"\)`),
}

// Read is a single statically-resolvable env access site.
type Read struct {
	Key  string
	File string
}

// ScanReads walks the module for .go and .memql files and extracts
// every direct env read.
func ScanReads(root string) ([]Read, error) {
	var reads []Read
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !scannable(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		patterns := goReadPatterns
		if strings.HasSuffix(path, ".memql") {
			patterns = memqlReadPatterns
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				key := m[1]
				if isExternal(key) {
					continue
				}
				reads = append(reads, Read{Key: key, File: rel})
			}
		}
		return nil
	})
	return reads, err
}

func skipDir(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "vendor", "gen", ".git", "node_modules", "testdata":
		return true
	}
	return false
}

func scannable(path string) bool {
	switch {
	case strings.HasSuffix(path, "_test.go"):
		return false
	case strings.HasSuffix(path, ".pb.go"):
		return false
	case strings.HasSuffix(path, ".go"):
		// Skip the scanner itself (its denylist literals are not reads).
		return !strings.Contains(path, filepath.Join("cmd", "envscan"))
	case strings.HasSuffix(path, ".memql"):
		return true
	}
	return false
}

// LoadRegistry loads the manifest from the repo path explicitly so the
// scan does not depend on MEMQL_REPO / embedded-snapshot resolution.
func LoadRegistry(root string) (*genesis.Manifest, error) {
	path := filepath.Join(root, "scripts", "secrets", "manifest.yaml")
	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return genesis.LoadManifestFromBytes(data, path)
	}
	return genesis.LoadManifest("")
}

// RegisteredSet returns the set of every registered env-var name.
func RegisteredSet(manifest *genesis.Manifest) map[string]bool {
	registered := map[string]bool{}
	for _, e := range manifest.AllEntries() {
		registered[e.Name] = true
	}
	return registered
}

// UniqueKeys returns the sorted, de-duplicated set of read keys.
func UniqueKeys(reads []Read) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range reads {
		if !seen[r.Key] {
			seen[r.Key] = true
			out = append(out, r.Key)
		}
	}
	sort.Strings(out)
	return out
}

// UnregisteredKeys returns the sorted reads that are missing from the
// registry (forward-drift candidates).
func UnregisteredKeys(reads []Read, registered map[string]bool) []string {
	var out []string
	for _, k := range UniqueKeys(reads) {
		if !registered[k] {
			out = append(out, k)
		}
	}
	return out
}

// PrintReads writes every detected read (key\tfiles) to w.
func PrintReads(w *strings.Builder, reads []Read) {
	byKey := map[string][]string{}
	for _, r := range reads {
		byKey[r.Key] = append(byKey[r.Key], r.File)
	}
	for _, k := range UniqueKeys(reads) {
		files := dedupe(byKey[k])
		fmt.Fprintf(w, "%s\t%s\n", k, strings.Join(files, ", "))
	}
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// Result is the outcome of a drift check: the forward-drift keys (read
// but unregistered), the reverse-drift keys (registered but stale), and
// summary counts.
type Result struct {
	Unregistered []string // forward drift
	Stale        []string // reverse drift
	ReadCount    int
	RegistrySize int
}

// OK reports whether the check found no drift in either direction.
func (r Result) OK() bool {
	return len(r.Unregistered) == 0 && len(r.Stale) == 0
}

// Violations is the total number of drift violations across both
// directions.
func (r Result) Violations() int {
	return len(r.Unregistered) + len(r.Stale)
}

// CheckDrift runs the full drift check over a module root and returns a
// Result. It loads the registry, scans reads, and walks the repo corpus
// for reverse drift. Any I/O failure is returned as an error.
func CheckDrift(root string) (Result, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Result{}, fmt.Errorf("resolve root: %w", err)
	}

	reads, err := ScanReads(absRoot)
	if err != nil {
		return Result{}, fmt.Errorf("scan reads: %w", err)
	}

	manifest, err := LoadRegistry(absRoot)
	if err != nil {
		return Result{}, fmt.Errorf("load registry: %w", err)
	}
	registered := RegisteredSet(manifest)

	res := Result{
		Unregistered: UnregisteredKeys(reads, registered),
		ReadCount:    len(UniqueKeys(reads)),
		RegistrySize: len(manifest.AllEntries()),
	}

	corpus, excluded, err := repoCorpus(absRoot)
	if err != nil {
		return Result{}, fmt.Errorf("build corpus: %w", err)
	}
	// The exclusion IS the reverse-drift check, so a miss has to be loud.
	//
	// If a registry file is moved or renamed it silently re-enters the
	// corpus, every entry regains a self-reference, and the entire reverse
	// direction goes quiet with the gate still green -- memql#2971
	// returning by a different route. Erroring here turns that into a
	// build failure naming the file it could not find.
	if len(excluded) != len(registryFiles) {
		return Result{}, fmt.Errorf(
			"reverse-drift corpus excluded %d of %d registry files (want %v, got %v): every "+
				"registry file must be excluded or a stale entry references itself and this "+
				"check goes silent. If a manifest moved, update registryFiles",
			len(excluded), len(registryFiles), registryFiles, excluded)
	}
	for _, e := range manifest.AllEntries() {
		if isExternal(e.Name) {
			continue
		}
		// A registry entry is "used" if its name appears anywhere in the
		// repo OUTSIDE the registry itself (code reads, k8s overlays,
		// .env templates, dsl). This tolerates env.NewEnvReader
		// prefix-composed full keys and k8s-only injection targets that no
		// Go literal reads directly.
		//
		// The corpus excludes every copy of the registry, so ANY occurrence
		// is a real reference and the threshold needs no off-by-one
		// allowance to explain. It read `< 2` with the comment
		// "1 = the manifest row itself", which stopped being true when the
		// embedded snapshot was added: two identical copies gave every
		// entry a corpus floor of 2, so the condition could never select
		// and reverse drift was dead in every state CI could reach
		// (memql#2971).
		if !strings.Contains(corpus, e.Name) {
			res.Stale = append(res.Stale, e.Name)
		}
	}
	sort.Strings(res.Stale)
	return res, nil
}

// registryFiles are every copy of the registry itself, repo-relative and
// slash-separated. They are excluded from the reverse-drift corpus,
// because an entry's own row is not a reference to it.
//
// There are TWO copies and they carry identical rows: the authored
// scripts/secrets/manifest.yaml (the source of truth) and the
// //go:embed snapshot component/genesis/manifest.yaml, which
// scripts/secrets/sync-embedded-manifest.sh regenerates verbatim and
// TestEmbeddedManifestInSync keeps in step. Missing the second one is
// what made the reverse check unsatisfiable (memql#2971), so CheckDrift
// hard-fails rather than proceeding if any entry here goes unmatched.
var registryFiles = []string{
	"scripts/secrets/manifest.yaml",
	"component/genesis/manifest.yaml",
}

// repoCorpus concatenates the text of every config-bearing file
// (.go / .memql / .yaml / .yml / .env*) so reverse-drift can check
// whether a registered name is referenced anywhere.
//
// It returns the registry files it EXCLUDED alongside the corpus. That
// second value is not bookkeeping: the caller compares it against
// registryFiles, because an exclusion that silently stops matching is
// indistinguishable from a clean tree.
func repoCorpus(root string) (string, []string, error) {
	registry := make(map[string]bool, len(registryFiles))
	for _, rel := range registryFiles {
		registry[rel] = true
	}

	var b strings.Builder
	var excluded []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, relErr := filepath.Rel(root, path); relErr == nil {
			if slash := filepath.ToSlash(rel); registry[slash] {
				excluded = append(excluded, slash)
				return nil
			}
		}
		base := filepath.Base(path)
		switch {
		case strings.HasSuffix(path, ".go"),
			strings.HasSuffix(path, ".memql"),
			strings.HasSuffix(path, ".yaml"),
			strings.HasSuffix(path, ".yml"),
			strings.HasPrefix(base, ".env"),
			strings.Contains(base, ".env"):
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.Write(data)
			b.WriteByte('\n')
		}
		return nil
	})
	sort.Strings(excluded)
	return b.String(), excluded, err
}
