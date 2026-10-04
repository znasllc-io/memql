package pipelines

import (
	"fmt"
	"regexp"
	"strings"
)

// globUnsupportedMeta are the metacharacters CompileGlob refuses: everything
// picomatch gives meaning to that this subset does not implement -- `[]`
// classes, `{}` braces, `()` `+` `@` extglobs and `\` escapes. It is
// scripts/ci/pathsfilter.go's unsupportedMeta less `?`, which this port adds.
const globUnsupportedMeta = `[]{}()+@\`

// CompileGlob compiles one path glob: `**` crosses directories (zero or
// more), `*` and `?` stay inside one segment, a leading `!` negates;
// patterns are anchored at the repository root.
//
// It is a port of the CI bridge's matcher, scripts/ci/pathsfilter.go's
// CompilePattern, which was differentially tested against picomatch 2.3.1
// under {dot: true} (what dorny/paths-filter runs), plus `?`: one rune that
// is not a separator. The two must agree on every pattern both accept, so a
// path bucket moved from ci.yml into a manifest means what it meant; the
// root module's parity test holds them together, which is why the structure
// below follows the original line for line rather than being rewritten.
//
//	pattern  := ["!"] segment ("/" segment)*
//	segment  := "**" | chars-with-optional-"*"-and-"?"
//
//	"**"  as a whole segment matches zero or more path segments; a trailing
//	      "/**" also matches the directory itself
//	"*"   inside a segment matches zero or more characters except "/"
//	"?"   inside a segment matches exactly one character except "/"
//	"!"   at position 0 negates the ENTIRE pattern
//
// A dot is not special (dot: true). Anything else is an error, never a
// guess: a glob the matcher cannot score is a bucket nobody can vouch for.
func CompileGlob(pattern string) (func(path string) bool, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	negated := false
	body := pattern
	if strings.HasPrefix(body, "!") {
		negated = true
		body = body[1:]
		if body == "" {
			return nil, fmt.Errorf("pattern %q: a bare %q negates nothing", pattern, "!")
		}
	}
	if strings.Contains(body, "!") {
		return nil, fmt.Errorf("pattern %q: %q is only supported at position 0", pattern, "!")
	}
	if i := strings.IndexAny(body, globUnsupportedMeta); i >= 0 {
		return nil, fmt.Errorf("pattern %q: unsupported glob character %q; the supported grammar "+
			"is **, * and ? and a leading !", pattern, string(body[i]))
	}

	segments := strings.Split(body, "/")
	var sb strings.Builder
	sb.WriteString("^")

	// needSep says whether the NEXT emitted segment must be preceded by a
	// separator. Both globstar forms supply their own, so they clear or skip
	// it rather than letting one be emitted twice (the original's
	// `gen/(?:/.*)?` defect, which matched nothing).
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
				// optional group. A lone `**` matches everything.
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
		sb.WriteString(globSegmentRegexp(seg))
		needSep = true
	}
	sb.WriteString("$")

	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, fmt.Errorf("pattern %q compiled to an invalid expression %q: %w", pattern, sb.String(), err)
	}
	if negated {
		return func(path string) bool { return !re.MatchString(path) }, nil
	}
	return re.MatchString, nil
}

// globSegmentRegexp translates one non-globstar segment, where `*` and `?`
// never cross a separator. The literal runs between them are quoted as BYTES,
// as the original quotes them: ranging over runes would turn an invalid UTF-8
// byte into U+FFFD and quietly accept a pattern the original refuses (the
// expression compiler rejects invalid UTF-8).
func globSegmentRegexp(seg string) string {
	var sb strings.Builder
	for i, part := range strings.Split(seg, "*") {
		if i > 0 {
			sb.WriteString("[^/]*")
		}
		for j, literal := range strings.Split(part, "?") {
			if j > 0 {
				sb.WriteString("[^/]")
			}
			sb.WriteString(regexp.QuoteMeta(literal))
		}
	}
	return sb.String()
}

// PathSet compiles a list of globs into one predicate: a path is in the set
// when some plain glob matches it and no `!` glob does. Order does not
// matter, and a list of only `!` globs is refused because it includes
// nothing.
//
// This is deliberately NOT paths-filter's reading. dorny/paths-filter ORs a
// filter's patterns, so a `!` glob in a list WIDENS it to every path that
// glob does not name -- ci.yml lives with that because its authors know the
// tool. A manifest is written by somebody who reads `!` as "except", and the
// paths-filter reading would quietly run a bucket's steps on nearly every
// change. Each single glob still means exactly what CompileGlob says.
func PathSet(globs []string) (func(path string) bool, error) {
	var include, exclude []func(string) bool
	for _, glob := range globs {
		if rest, negated := strings.CutPrefix(glob, "!"); negated {
			match, err := CompileGlob(rest)
			if err != nil {
				return nil, fmt.Errorf("pattern %q: %w", glob, err)
			}
			exclude = append(exclude, match)
			continue
		}
		match, err := CompileGlob(glob)
		if err != nil {
			return nil, err
		}
		include = append(include, match)
	}
	if len(include) == 0 {
		return nil, fmt.Errorf("the list names no path to include; a %q glob only narrows one that does", "!")
	}
	return func(path string) bool {
		for _, in := range include {
			if !in(path) {
				continue
			}
			for _, out := range exclude {
				if out(path) {
					return false
				}
			}
			return true
		}
		return false
	}, nil
}
