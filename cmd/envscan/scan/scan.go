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
// Reads are detected from statically-resolvable access sites only
// (os.Getenv / os.LookupEnv, the component/config env* helpers, and DSL
// env("...") literals). Go reads are found by parsing, so a key named
// through a constant -- os.Getenv(envBadgeGrantPerHour) -- resolves like
// a literal; see goast.go.
//
// What CANNOT be resolved is reported, never dropped: a site whose key is
// a parameter or a loop variable lands in Outcome.Unresolvable with its
// file:line, and every surface (-check, -list, -unresolvable, and the
// drift test's log line) states the count. A scanner that folded
// constants and then quietly omitted the remainder would rebuild the
// defect it was written to fix -- a clean number that is clean only about
// what the mechanism happened to see (memql#3818).
//
// A THIRD read class -- core/env.EnvReader -- is DETECTED AND COUNTED but
// deliberately not resolved. reader.String("HOST") names a suffix; the full
// key is that suffix under the prefix the reader's constructor was given,
// and finding it means tracing a reader value across parameters, struct
// fields and packages. So each site lands in Unresolvable with
// Kind == KindReaderPrefix and a reason naming the mechanism. It used to be
// detected as NOTHING, which made it the worst of the three: absent from
// the read count AND absent from the residual, with the limitation
// recorded only in this comment. See goast.go.
//
// # What this still does not see, stated so the total is not read as coverage
//
// A read through neither os.Getenv/os.LookupEnv, nor an env* helper, nor an
// EnvReader is still detected AT ALL -- not as a read, not as a residual --
// because nothing at the call site marks it as an env access. Two live
// shapes:
//
//   - an injected getter func value, used for testability:
//     get("MEMQL_VOICE_EXECUTOR", "realtime") in the voice-agent, where
//     `get` is a plain func parameter and so is indistinguishable from any
//     other two-argument call;
//   - a name table, like integrations/email's Host: "SMTP_HOST", where the
//     key is data rather than an argument.
//
// Registered keys in that class are still covered by the REVERSE direction
// (their name appears in the corpus), so they cannot go stale silently --
// they are simply not FORCED into the registry by the forward check.
// Unregistered ones are invisible, which is this defect's shape again with
// a different mechanism, and it is the honest remaining gap. Closing it
// means deciding which callees count as env readers, so it is a separate
// task rather than a widened match here.
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

// ownedPreConvention lists keys that ARE memQL-owned configuration but do not
// carry the MEMQL_ prefix. They are exempt from FORWARD drift and from nothing
// else -- CheckDrift returns them separately and every report PRINTS them, so
// the exemption is a line a reader can see rather than an absence.
//
// They are NOT in `external`, and that distinction is the point. `external`
// means "not memQL's variable"; these are memQL's variables, read by
// component/memql's engine config and component/server's HTTP router. Putting
// them there would be false, and would also exempt them from the reverse
// direction.
//
// EMPTY, AND THAT IS THE RESOLUTION -- not a list that happens to have nothing
// in it (memql#3831).
//
// It held six names: MEMORY_ENGINE_MAX_RESULTS / _MAX_WINDOW /
// _DEFAULT_LIST_CAP / _CACHE_MAX_ITEMS, CACHE_MAX_TTL and SERVER_PUBLIC_PATH.
// They could not be registered, because component/genesis's
// TestOwnedVarsArePrefixed (Epic 7.3 / memql#2106) fails on any
// non-MEMQL_-prefixed entry that is not a legacy alias, and none of the six was
// one. So registering them reddened that gate and omitting them reddened this
// one: there was no state of the registry in which both held.
//
// The exit was never a better exemption. It was a RENAME -- MEMQL_ names, the
// old names recorded in genesis.LegacyAliases so an operator's existing
// configuration keeps working through the boot-time shim, and the reading code
// updated. All six are registered now, so the contradiction is dissolved rather
// than tolerated on one side of it.
//
// What the list meant about that gate is worth keeping: TestOwnedVarsArePrefixed
// passed for as long as it did because the registry could not SEE these
// variables -- the same shape as the defect memql#3818 fixed, one layer down.
// Its premise ("every owned var is prefixed or aliased") was never true; it was
// unfalsified. It is true now, and falsifiable.
//
// The MECHANISM stays, and stays reported: every run still prints
// `0 unregistered-by-exemption`, which is a claim a reader can act on in a way
// that removing the counter would not be. If a seventh pre-convention name is
// ever found, it lands here, becomes visible immediately, and gets renamed.
//
// Do NOT add a name here to silence a drift failure. A newly-written env var
// must be MEMQL_-prefixed and registered; nothing predating the convention
// remains to be discovered in this tree, which is what
// TestPreConventionExemptionIsEmpty asserts.
var ownedPreConvention = map[string]bool{}

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

// memqlReadPatterns match a DSL provider-auth read. The bare env("...")
// form is DSL-only -- in Go it appears solely in doc-comments, so it is
// scoped to .memql files to avoid false positives.
var memqlReadPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\benv\(\s*"([A-Z][A-Z0-9_]+)"\)`),
}

// Read is a single statically-resolved env access site.
type Read struct {
	Key  string
	File string
}

// ScanReads walks the module and returns every resolved env read
// together with every read-shaped site whose key could not be resolved.
//
// Go files are parsed (constants fold; see goast.go); .memql files keep
// the regex scan, because the DSL env("...") form is literal-only by
// construction.
//
// The name outlived a rename to `Scan` for a reason worth knowing: it is
// a node in the committed architecture model, and
// TestArchitectureModelIsNotStale reds on a symbol that disappears
// (memql#3050). Renaming it means regenerating that model in the same
// change.
func ScanReads(root string) (Outcome, error) {
	out, err := scanGo(root)
	if err != nil {
		return Outcome{}, err
	}

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") || !scannable(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, re := range memqlReadPatterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				key := m[1]
				if isExternal(key) {
					continue
				}
				out.Reads = append(out.Reads, Read{Key: key, File: filepath.ToSlash(rel)})
			}
		}
		return nil
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
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

// CountKind returns how many residual sites are of one kind.
//
// The split is reported alongside the total because the two kinds want
// DIFFERENT fixes: an unresolved key is fixed at the CALL SITE (name the key
// with a constant), while a reader-prefix site is fixed in THIS SCANNER (teach
// it to trace a reader's constructor prefix). A single number hides which of
// those a reader is looking at.
func CountKind(sites []Unresolvable, kind UnresolvableKind) int {
	n := 0
	for _, u := range sites {
		if u.Kind == kind {
			n++
		}
	}
	return n
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

// PrintUnresolvable writes the residual -- every read-shaped site whose
// key could not be resolved -- one per line, as
// `file:line\tcall(arg)\treason`.
//
// This exists so the residual is legible rather than merely counted: the
// reader can open the line and judge whether it is a genuinely dynamic
// key (the inbound per-source knobs), a helper that only shares the env*
// name shape (envSuffix, envOptionsToArgsFn), or a var that ought to be
// read through a constant so the gate can see it.
func PrintUnresolvable(w *strings.Builder, sites []Unresolvable) {
	for _, u := range sites {
		fmt.Fprintln(w, u.String())
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
// but unregistered), the reverse-drift keys (registered but stale), the
// residual the scan could not resolve, and summary counts.
type Result struct {
	Unregistered []string // forward drift
	Stale        []string // reverse drift
	// Unresolvable is the residual: read-shaped call sites whose key is
	// a parameter, a loop variable, or a computed value. It is NOT drift
	// and does not fail the check -- it is the part of the surface the
	// mechanism cannot speak for, carried here so every report states it
	// instead of implying coverage it does not have (memql#3818).
	Unresolvable []Unresolvable
	// ExemptUnprefixed is the ownedPreConvention set that this scan
	// actually read: memQL-owned keys that a registry entry cannot yet
	// name (see ownedPreConvention). Reported, not failed -- and reported
	// precisely so the exemption cannot become the thing nobody sees.
	ExemptUnprefixed []string
	ReadCount        int
	RegistrySize     int
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

	out, err := ScanReads(absRoot)
	if err != nil {
		return Result{}, fmt.Errorf("scan reads: %w", err)
	}

	manifest, err := LoadRegistry(absRoot)
	if err != nil {
		return Result{}, fmt.Errorf("load registry: %w", err)
	}
	registered := RegisteredSet(manifest)

	res := Result{
		Unresolvable: out.Unresolvable,
		ReadCount:    len(UniqueKeys(out.Reads)),
		RegistrySize: len(manifest.AllEntries()),
	}
	// Split the unregistered reads into real forward drift and the
	// pre-convention exemption, so the exemption is a printed line rather
	// than a name that quietly stopped counting.
	for _, k := range UnregisteredKeys(out.Reads, registered) {
		if ownedPreConvention[k] {
			res.ExemptUnprefixed = append(res.ExemptUnprefixed, k)
			continue
		}
		res.Unregistered = append(res.Unregistered, k)
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
	//
	// Compared as a SET rather than a count: `len(excluded) != len(registryFiles)`
	// is satisfied by any two exclusions, so it would pass while excluding the
	// wrong files. The predicate has to name what is missing, not how much.
	if missing := missingRegistryFiles(excluded); len(missing) > 0 {
		return Result{}, fmt.Errorf(
			"reverse-drift corpus did not exclude registry file(s) %v (excluded %v of want %v): "+
				"every registry file must be excluded or a stale entry references itself and "+
				"this check goes silent. If a manifest moved, update registryFiles",
			missing, excluded, registryFiles)
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
		// The corpus excludes every copy of the registry, so an occurrence
		// is a real reference and the threshold needs no off-by-one
		// allowance to explain. It read `< 2` with the comment
		// "1 = the manifest row itself", which stopped being true when the
		// embedded snapshot was added: two identical copies gave every
		// entry a corpus floor of 2, so the condition could never select
		// and reverse drift was dead in every state CI could reach
		// (memql#2971).
		//
		// The occurrence must be WHOLE-WORD. A plain substring match is not
		// enough here, and this is the second way #2971's defect survived
		// rather than a refinement: 87 of the 299 entries are proper
		// substrings of another entry, and every one of them is a legacy
		// alias whose own manifest row says "DEPRECATED legacy alias for
		// MEMQL_X; remove after operators migrate". Deleting that alias is
		// exactly the edit this gate exists to catch, and under a substring
		// match the surviving MEMQL_X kept the alias looking referenced
		// forever -- measured by dropping one alias row from
		// component/genesis/legacyalias.go, which left that name with zero
		// real references while the gate still reported no drift.
		//
		// Deliberately no live registry name in this comment: repoCorpus
		// ingests .go files, this file included, so naming one here would
		// itself count as the reference and keep it out of Stale forever.
		if !referencedWholeWord(corpus, e.Name) {
			res.Stale = append(res.Stale, e.Name)
		}
	}
	sort.Strings(res.Stale)
	return res, nil
}

// referencedWholeWord reports whether name occurs in corpus as a whole
// token -- not as part of a longer env-var name.
//
// Env keys are [A-Z0-9_], so the boundary test is simply that the
// character on either side of a hit is not one of those (and not a
// lowercase letter, so a Go identifier like myMEMQL_FOOBar is not a
// reference either).
//
// This is what makes the reverse check satisfiable for the 87 entries
// that are proper substrings of another entry. All of them are legacy
// aliases queued for removal, so they are precisely the rows whose
// staleness the gate is meant to report (memql#2971).
func referencedWholeWord(corpus, name string) bool {
	if name == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(corpus[i:], name)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(name)
		if !isEnvNameByte(corpus, start-1) && !isEnvNameByte(corpus, end) {
			return true
		}
		i = start + 1
	}
}

// isEnvNameByte reports whether the byte at idx could be part of an env
// key or a surrounding identifier. Out-of-range counts as a boundary.
func isEnvNameByte(s string, idx int) bool {
	if idx < 0 || idx >= len(s) {
		return false
	}
	c := s[idx]
	return c == '_' ||
		(c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9')
}

// missingRegistryFiles returns the registryFiles entries absent from
// excluded. Set difference rather than a length compare, so the caller
// can name what went missing instead of only that something did.
func missingRegistryFiles(excluded []string) []string {
	got := make(map[string]bool, len(excluded))
	for _, e := range excluded {
		got[e] = true
	}
	var missing []string
	for _, want := range registryFiles {
		if !got[want] {
			missing = append(missing, want)
		}
	}
	return missing
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
			slash := filepath.ToSlash(rel)
			if registry[slash] {
				excluded = append(excluded, slash)
				return nil
			}
			// The scanner's own source is not a reference, for the same
			// reason scannable() already skips it on the read side: this
			// package names env vars in comments and denylist literals.
			// Measured while fixing memql#2971 -- a var named in a comment
			// HERE kept itself out of Stale, which is the defect wearing
			// the reviewer's clothes.
			if strings.HasPrefix(slash, "cmd/envscan/") {
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
