// Command envscan is the shared env-var classifier for Epic 7
// (memql#2103). It is the single source of truth for "which env vars
// does the code read", used both by the registry audit (7.1 / #2104)
// and by the CI drift gate (7.2 / #2105) so the two can never diverge.
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
//
// Usage:
//
//	envscan -list              # print every detected read (key\tfile)
//	envscan -check             # exit non-zero on forward or reverse drift
//	envscan -unregistered      # print only reads missing from the registry
//	envscan -root <dir>        # module root (default: cwd)
package main

import (
	"flag"
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

type read struct {
	key  string
	file string
}

func main() {
	var (
		root           string
		doList         bool
		doCheck        bool
		doUnregistered bool
	)
	flag.StringVar(&root, "root", ".", "module root to scan")
	flag.BoolVar(&doList, "list", false, "print every detected env read")
	flag.BoolVar(&doCheck, "check", false, "exit non-zero on registry drift (forward + reverse)")
	flag.BoolVar(&doUnregistered, "unregistered", false, "print reads missing from the registry")
	flag.Parse()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		fail("resolve root: %v", err)
	}

	reads, err := scanReads(absRoot)
	if err != nil {
		fail("scan: %v", err)
	}

	manifest, err := loadRegistry(absRoot)
	if err != nil {
		fail("load registry: %v", err)
	}
	registered := map[string]bool{}
	for _, e := range manifest.AllEntries() {
		registered[e.Name] = true
	}

	switch {
	case doList:
		printReads(reads)
	case doUnregistered:
		for _, k := range unregisteredKeys(reads, registered) {
			fmt.Println(k)
		}
	case doCheck:
		os.Exit(checkDrift(absRoot, reads, manifest, registered))
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// scanReads walks the module for .go and .memql files and extracts
// every direct env read.
func scanReads(root string) ([]read, error) {
	var reads []read
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
				reads = append(reads, read{key: key, file: rel})
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

// loadRegistry loads the manifest from the repo path explicitly so the
// scan does not depend on MEMQL_REPO / embedded-snapshot resolution.
func loadRegistry(root string) (*genesis.Manifest, error) {
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

func uniqueKeys(reads []read) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range reads {
		if !seen[r.key] {
			seen[r.key] = true
			out = append(out, r.key)
		}
	}
	sort.Strings(out)
	return out
}

func unregisteredKeys(reads []read, registered map[string]bool) []string {
	var out []string
	for _, k := range uniqueKeys(reads) {
		if !registered[k] {
			out = append(out, k)
		}
	}
	return out
}

func printReads(reads []read) {
	byKey := map[string][]string{}
	for _, r := range reads {
		byKey[r.key] = append(byKey[r.key], r.file)
	}
	for _, k := range uniqueKeys(reads) {
		files := dedupe(byKey[k])
		fmt.Printf("%s\t%s\n", k, strings.Join(files, ", "))
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

// checkDrift reports forward drift (reads missing from registry) and
// reverse drift (registry entries that appear nowhere in the repo).
// Returns a process exit code.
func checkDrift(root string, reads []read, manifest *genesis.Manifest, registered map[string]bool) int {
	violations := 0

	unreg := unregisteredKeys(reads, registered)
	if len(unreg) > 0 {
		violations += len(unreg)
		fmt.Fprintln(os.Stderr, "ERROR: env vars read in code but NOT registered in scripts/secrets/manifest.yaml:")
		for _, k := range unreg {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}

	corpus, err := repoCorpus(root)
	if err != nil {
		fail("build corpus: %v", err)
	}
	var stale []string
	for _, e := range manifest.AllEntries() {
		if isExternal(e.Name) {
			continue
		}
		// A registry entry is "used" if its name appears anywhere in the
		// repo (code reads, k8s overlays, .env templates, dsl). This
		// tolerates env.NewEnvReader prefix-composed full keys and
		// k8s-only injection targets that no Go literal reads directly.
		if strings.Count(corpus, e.Name) < 2 { // 1 = the manifest row itself
			stale = append(stale, e.Name)
		}
	}
	if len(stale) > 0 {
		violations += len(stale)
		fmt.Fprintln(os.Stderr, "ERROR: registry entries that appear nowhere in the repo (stale):")
		sort.Strings(stale)
		for _, k := range stale {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
	}

	if violations == 0 {
		fmt.Printf("envscan: OK -- %d reads, %d registry entries, no drift\n",
			len(uniqueKeys(reads)), len(manifest.AllEntries()))
		return 0
	}
	fmt.Fprintf(os.Stderr, "envscan: FAIL -- %d drift violation(s)\n", violations)
	return 1
}

// repoCorpus concatenates the text of every config-bearing file
// (.go / .memql / .yaml / .yml / .env*) so reverse-drift can check
// whether a registered name is referenced anywhere.
func repoCorpus(root string) (string, error) {
	var b strings.Builder
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
	return b.String(), err
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "envscan: "+format+"\n", args...)
	os.Exit(2)
}
