package bundle

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultAllowlist is the boundary allowlist, relative to the repository root.
const DefaultAllowlist = "docs/public_boundary_allowlist.toml"

// AllowEntry lets one known link through the boundary check until the page
// is fixed. It names the link by the page that carries it, its target as
// written, and the rule it breaks. One entry covers every occurrence of that
// target on that page.
type AllowEntry struct {
	File   string // repository-relative page path
	Target string // the link target as written
	Rule   string
	Reason string // optional
	Line   int    // the entry's line in the allowlist file
}

// Allowlist is the parsed allowlist file.
type Allowlist struct {
	Path    string
	Entries []AllowEntry
}

// LoadAllowlist reads the allowlist at path. A missing file is an empty list.
func LoadAllowlist(path string) (Allowlist, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Allowlist{Path: path}, nil
	}
	if err != nil {
		return Allowlist{}, err
	}
	return ParseAllowlist(path, data)
}

// ParseAllowlist reads the allowlist's format: the subset of TOML the
// repository's other allowlist (.gitleaks.toml) is written in, and nothing
// more --
//
//	# a comment
//	[[allow]]
//	file   = "docs/public/build/build-tags.md"
//	target = "../../internal/design/platform-consolidation.md"
//	rule   = "leaves-public"
//	reason = "optional"
//
// Basic strings only. Anything else -- another table, an unknown key, a
// duplicate entry, a rule no link can break -- is refused rather than
// guessed at, because a list that silently read less than it said would let
// links through that nobody allowed.
func ParseAllowlist(name string, data []byte) (Allowlist, error) {
	a := Allowlist{Path: name}
	var cur *AllowEntry
	seen := map[[3]string]int{}
	finish := func() error {
		if cur == nil {
			return nil
		}
		if cur.File == "" || cur.Target == "" || cur.Rule == "" {
			return fmt.Errorf("%s:%d: an [[allow]] entry needs file, target and rule", name, cur.Line)
		}
		if !linkRules[cur.Rule] {
			return fmt.Errorf("%s:%d: rule %q is not a link rule an entry can allow (leaves-public, unselected-target, unrewritable-target, missing-target)", name, cur.Line, cur.Rule)
		}
		key := [3]string{cur.File, cur.Target, cur.Rule}
		if first, dup := seen[key]; dup {
			return fmt.Errorf("%s:%d: duplicates the entry at line %d", name, cur.Line, first)
		}
		seen[key] = cur.Line
		a.Entries = append(a.Entries, *cur)
		cur = nil
		return nil
	}
	for i, raw := range strings.Split(string(data), "\n") {
		n := i + 1
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[[allow]]" {
			if err := finish(); err != nil {
				return Allowlist{}, err
			}
			cur = &AllowEntry{Line: n}
			continue
		}
		if strings.HasPrefix(line, "[") {
			return Allowlist{}, fmt.Errorf("%s:%d: only [[allow]] tables belong here, got %s", name, n, line)
		}
		if cur == nil {
			return Allowlist{}, fmt.Errorf("%s:%d: a key outside an [[allow]] entry", name, n)
		}
		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			return Allowlist{}, fmt.Errorf("%s:%d: expected key = \"value\"", name, n)
		}
		val, err := basicString(strings.TrimSpace(rest))
		if err != nil {
			return Allowlist{}, fmt.Errorf("%s:%d: %v", name, n, err)
		}
		switch strings.TrimSpace(key) {
		case "file":
			cur.File = val
		case "target":
			cur.Target = val
		case "rule":
			cur.Rule = val
		case "reason":
			cur.Reason = val
		default:
			return Allowlist{}, fmt.Errorf("%s:%d: unknown key %q (file, target, rule, reason)", name, n, strings.TrimSpace(key))
		}
	}
	if err := finish(); err != nil {
		return Allowlist{}, err
	}
	return a, nil
}

// basicString reads one TOML basic string, "...", with an optional trailing
// comment.
func basicString(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		return "", fmt.Errorf("value must be a double-quoted string, got %s", s)
	}
	end := 1
	for end < len(s) {
		if s[end] == '\\' {
			end += 2
			continue
		}
		if s[end] == '"' {
			break
		}
		end++
	}
	if end >= len(s) {
		return "", fmt.Errorf("unterminated string %s", s)
	}
	if tail := strings.TrimSpace(s[end+1:]); tail != "" && !strings.HasPrefix(tail, "#") {
		return "", fmt.Errorf("unexpected text after the string: %s", tail)
	}
	v, err := strconv.Unquote(s[:end+1])
	if err != nil {
		return "", fmt.Errorf("bad string %s: %v", s[:end+1], err)
	}
	return v, nil
}

// apply splits link violations into allowed and not, and returns a violation
// for every entry that matched nothing.
func (a Allowlist) apply(vs []Violation) (unallowed []Violation, allowed map[[3]string]bool, stale []Violation) {
	allowed = map[[3]string]bool{}
	index := map[[3]string]bool{}
	for _, e := range a.Entries {
		index[[3]string{e.File, e.Target, e.Rule}] = true
	}
	used := map[[3]string]bool{}
	for _, v := range vs {
		key := [3]string{v.File, v.Target, v.Rule}
		if linkRules[v.Rule] && index[key] {
			used[key] = true
			allowed[key] = true
			continue
		}
		unallowed = append(unallowed, v)
	}
	for _, e := range a.Entries {
		key := [3]string{e.File, e.Target, e.Rule}
		if used[key] {
			continue
		}
		stale = append(stale, Violation{
			File: a.Path, Line: e.Line, Target: e.Target, Rule: RuleStaleAllowlistEntry,
			Detail: fmt.Sprintf("%s no longer carries a %s link to this target; delete the entry", e.File, e.Rule),
		})
	}
	return unallowed, allowed, stale
}
