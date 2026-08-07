// Package ci hosts the static guards that verify this repository's CI
// configuration is what it claims to be.
//
// This file is the shared matcher those guards need: a faithful, fail-closed
// reimplementation of how `dorny/paths-filter` decides whether a changed file
// belongs to a named filter bucket (znasllc-io/memql#3222, memql#3223).
//
// # Why a reimplementation, and why it has to be exact
//
// A guard that approximates the matcher is worse than no guard, because it
// disagrees with CI in whichever direction the approximation errs -- either it
// fails on routing that actually works, and gets suppressed, or it passes on
// routing that does not, and reports the fail-open as clean.
//
// So the semantics below are not inferred from the action's README. They are
// read off the pinned action's source at the SHA this repo pins,
// `dorny/paths-filter@7b450fff21473bca461d4b92ce414b9d0420d706` (v4.0.2),
// `src/filter.ts`:
//
//	const MatchOptions = {dot: true}
//	...
//	return [{status: undefined, isMatch: picomatch(item, MatchOptions)}]
//	...
//	private isMatch(file, patterns) {
//	  if (this.filterConfig?.predicateQuantifier === 'every') return patterns.every(aPredicate)
//	  else return patterns.some(aPredicate)
//	}
//
// Three consequences, all load-bearing:
//
//  1. Each pattern in a bucket is compiled SEPARATELY, into its own matcher.
//  2. The per-pattern results are combined with `.some()` -- a logical OR.
//     `.github/workflows/ci.yml` does not set `predicate-quantifier`, so the
//     default SOME applies.
//  3. `dot: true`, so a leading dot in a path segment is not special.
//
// Point 1 combined with point 2 is the whole of memql#3222: a `!negated`
// pattern in a list is its own OR-ed term meaning "anything that is not this",
// so it ADDS matches rather than subtracting them. Exclusion is not expressible
// in a paths-filter pattern list at all. `TestNoNegatedPatternWidensItsBucket`
// is what holds that line.
//
// The action's own package-lock at that SHA pins `picomatch` to exactly 2.3.1.
//
// # Fail-closed grammar
//
// picomatch implements a very large glob grammar -- braces, extglobs, POSIX
// classes, regex passthrough. Reimplementing all of it correctly is not
// realistic, and silently mis-implementing a corner of it is precisely the
// failure this package exists to prevent.
//
// So CompilePattern implements a deliberately small subset and REFUSES
// anything outside it. Every pattern in `ci.yml` today is inside the subset,
// and every construct in the subset has been differentially tested against
// real picomatch 2.3.1 over the full tracked-file list -- see
// pathsfilter_test.go. If someone writes a `{a,b}` brace pattern in a bucket,
// the guards fail loudly with "unsupported" rather than quietly scoring it
// wrong.
package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// unsupportedMeta are the glob metacharacters CompilePattern refuses. They are
// all things picomatch gives meaning to and this subset does not implement:
// `?` single-char, `[]` classes, `{}` braces, `()` + `+` + `@` extglobs, and
// `\` escapes.
const unsupportedMeta = `?[]{}()+@\`

// CompilePattern turns one paths-filter pattern into a matcher with the same
// semantics as picomatch 2.3.1 under `{dot: true}`.
//
// Supported grammar:
//
//	pattern  := ["!"] segment ("/" segment)*
//	segment  := "**" | chars-with-optional-"*"
//
//	"**"  as a whole segment matches zero or more path segments
//	"*"   inside a segment matches zero or more characters except "/"
//	"!"   at position 0 negates the ENTIRE pattern (see the package comment --
//	      under OR-combination this widens the bucket, it does not narrow it)
//
// Anything else is an error. Callers are expected to treat that error as a
// test failure, not to skip the pattern: a pattern the guard cannot score is
// a bucket the guard cannot vouch for.
func CompilePattern(pattern string) (func(string) bool, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	negated := false
	body := pattern
	if strings.HasPrefix(body, "!") {
		negated = true
		body = body[1:]
		if body == "" {
			return nil, fmt.Errorf("pattern %q: bare %q negates nothing", pattern, "!")
		}
	}
	if strings.Contains(body, "!") {
		return nil, fmt.Errorf("pattern %q: %q is only supported at position 0", pattern, "!")
	}
	if i := strings.IndexAny(body, unsupportedMeta); i >= 0 {
		return nil, fmt.Errorf(
			"pattern %q: unsupported glob metacharacter %q. This matcher implements a small, "+
				"differentially-tested subset of picomatch and refuses the rest rather than "+
				"scoring it wrong -- either rewrite the pattern within the subset, or extend "+
				"CompilePattern and add oracle cases for the new construct",
			pattern, string(body[i]))
	}

	segments := strings.Split(body, "/")
	var sb strings.Builder
	sb.WriteString("^")

	// needSep tracks whether the NEXT emitted segment must be preceded by a
	// separator. Both globstar forms supply their own, so they clear or skip
	// it rather than letting one be emitted twice -- writing the separator
	// unconditionally produced `gen/(?:/.*)?`, which matches nothing, and the
	// differential test against real picomatch is what surfaced it.
	needSep := false

	for i, seg := range segments {
		last := i == len(segments)-1

		if strings.Contains(seg, "**") {
			if seg != "**" {
				return nil, fmt.Errorf(
					"pattern %q: %q must be a whole path segment; %q mixes it with other characters",
					pattern, "**", seg)
			}
			if last {
				// A trailing `/**` matches the named directory itself as well
				// as everything under it, so the separator belongs INSIDE the
				// optional group. A lone `**` (the whole pattern) matches
				// everything.
				if i > 0 {
					sb.WriteString("(?:/.*)?")
				} else {
					sb.WriteString(".*")
				}
				continue
			}
			// A non-final `**/` consumes zero or more whole segments,
			// INCLUDING zero -- which is why `**/*.go` matches a root-level
			// `main.go`. The group swallows its own trailing separator.
			if needSep {
				sb.WriteString("/")
			}
			sb.WriteString("(?:[^/]*/)*")
			needSep = false
			continue
		}

		if needSep {
			sb.WriteString("/")
		}
		sb.WriteString(segmentRegexp(seg))
		needSep = true
	}
	sb.WriteString("$")

	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q compiled to invalid regexp %q: %w", pattern, sb.String(), err)
	}
	if negated {
		return func(path string) bool { return !re.MatchString(path) }, nil
	}
	return re.MatchString, nil
}

// segmentRegexp translates one non-globstar segment, where `*` is
// within-segment only.
// The separator is chosen on the part INDEX, not on how much has been written
// so far: a segment like `*.svg` splits to ["", ".svg"], whose first part is
// empty, and keying off the buffer length silently dropped the `[^/]*` for
// every pattern beginning with `*`.
func segmentRegexp(seg string) string {
	var sb strings.Builder
	for i, part := range strings.Split(seg, "*") {
		if i > 0 {
			sb.WriteString("[^/]*")
		}
		sb.WriteString(regexp.QuoteMeta(part))
	}
	return sb.String()
}

// BucketMatcher is a compiled filter bucket: the per-pattern matchers, kept
// separate exactly as the action keeps them.
type BucketMatcher struct {
	Name     string
	Patterns []string
	matchers []func(string) bool
}

// CompileBucket compiles every pattern in a bucket. It returns an error on the
// first pattern outside the supported grammar.
func CompileBucket(name string, patterns []string) (*BucketMatcher, error) {
	b := &BucketMatcher{Name: name, Patterns: patterns}
	for _, p := range patterns {
		m, err := CompilePattern(p)
		if err != nil {
			return nil, fmt.Errorf("bucket %q: %w", name, err)
		}
		b.matchers = append(b.matchers, m)
	}
	return b, nil
}

// Match reports whether the bucket is true for path, using the action's
// `.some()` combination.
func (b *BucketMatcher) Match(path string) bool {
	for _, m := range b.matchers {
		if m(path) {
			return true
		}
	}
	return false
}

// MatchPositiveOnly reports whether the bucket is true for path considering
// only its NON-negated patterns.
//
// The gap between this and Match is the memql#3222 defect made measurable: a
// negation can only ever add matches under OR, so if the two disagree the
// bucket is wider than its author wrote it to be.
func (b *BucketMatcher) MatchPositiveOnly(path string) bool {
	for i, p := range b.Patterns {
		if strings.HasPrefix(p, "!") {
			continue
		}
		if b.matchers[i](path) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Reading the workflow
// ---------------------------------------------------------------------------

// RepoRoot resolves the repository root from this file's own location, so the
// guards do not depend on the working directory `go test` happens to use.
func RepoRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// CIWorkflowPath is the workflow the guards read.
func CIWorkflowPath() string {
	return filepath.Join(RepoRoot(), ".github", "workflows", "ci.yml")
}

// ParseChangesFilters returns the `changes` job's filter buckets, in
// declaration order-independent map form.
//
// It fails rather than returning an empty map when the block cannot be found:
// a guard that silently verifies nothing is the failure mode this whole
// package exists to prevent.
func ParseChangesFilters(workflowYAML []byte) (map[string][]string, error) {
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				With struct {
					Filters string `yaml:"filters"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowYAML, &wf); err != nil {
		return nil, fmt.Errorf("parse ci.yml: %w", err)
	}

	var filtersYAML string
	for _, s := range wf.Jobs["changes"].Steps {
		if s.With.Filters != "" {
			filtersYAML = s.With.Filters
			break
		}
	}
	if filtersYAML == "" {
		return nil, fmt.Errorf(
			"no `filters:` block found on the `changes` job -- this guard cannot verify what it " +
				"was written to verify, so it fails closed rather than reporting a clean tree")
	}

	var buckets map[string][]string
	if err := yaml.Unmarshal([]byte(filtersYAML), &buckets); err != nil {
		return nil, fmt.Errorf("parse the changes job's filters block: %w", err)
	}
	if len(buckets) == 0 {
		return nil, fmt.Errorf("parsed zero filter buckets from the `changes` job")
	}
	return buckets, nil
}

// consumedRef matches the two spellings a lane can use to read a bucket:
// `needs.changes.outputs.go` and `needs.changes.outputs['server-grpc']` (the
// bracket form is required for names containing a hyphen).
var consumedRef = regexp.MustCompile(`needs\.changes\.outputs(?:\.([A-Za-z0-9_-]+)|\['([^']+)'\])`)

// ConsumedBuckets returns the buckets some lane's `if:` or `env:` actually
// reads.
//
// This distinction is the point. A bucket that is declared but wired to
// nothing routes NOTHING, so it cannot count as coverage for any file --
// scoring it as coverage is how a file with no real CI looks verified
// (memql#3222, memql#3223). The target-module buckets from memql#3163 are
// deliberately in that state today.
func ConsumedBuckets(workflowYAML []byte) map[string]bool {
	consumed := map[string]bool{}
	for _, m := range consumedRef.FindAllStringSubmatch(string(workflowYAML), -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		// The `changes` job's own `outputs:` block references every bucket by
		// definition; only a DOWNSTREAM lane reading it counts as consumption.
		consumed[name] = true
	}
	return consumed
}

// SortedKeys is a small helper so failure messages are deterministic.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReadCIWorkflow reads ci.yml from disk.
func ReadCIWorkflow() ([]byte, error) {
	return os.ReadFile(CIWorkflowPath())
}
