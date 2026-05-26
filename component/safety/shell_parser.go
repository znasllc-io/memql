package safety

import (
	"path/filepath"
	"strings"
)

// shellSeparators are the operators that introduce a new command
// segment in a compound shell command. Matched OUTSIDE quoted
// strings only -- a literal `;` inside `"hello;world"` is not a
// separator.
//
// Order matters for the matcher: multi-character separators
// (`&&` / `||`) must be checked before their single-character
// prefixes (`&` / `|`) so we don't split `&&` as `& &`.
var shellSeparators = []string{"&&", "||", "|", ";", "&", "\n"}

// SegmentCommand splits cmd into command segments along the shell
// separators (`&& || | ; & \n`), while respecting single- and
// double-quoted strings (separators inside quotes don't split). It
// does NOT decode subshells, backticks, here-docs, or escapes -- use
// ContainsSubshell to detect those before relying on segment-level
// rules.
//
// Empty input or a command with no non-whitespace content returns a
// nil slice. Leading/trailing whitespace is trimmed from each
// segment; empty segments (e.g. trailing `;`) are dropped.
func SegmentCommand(cmd string) []string {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	var segs []string
	var cur strings.Builder
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s != "" {
			segs = append(segs, s)
		}
	}
	inSingle, inDouble := false, false
	i := 0
	for i < len(cmd) {
		c := cmd[i]
		// Quote handling first -- separators inside quotes are
		// literal characters, not segmentation points.
		if !inDouble && c == '\'' {
			inSingle = !inSingle
			cur.WriteByte(c)
			i++
			continue
		}
		if !inSingle && c == '"' {
			inDouble = !inDouble
			cur.WriteByte(c)
			i++
			continue
		}
		if inSingle || inDouble {
			cur.WriteByte(c)
			i++
			continue
		}
		// Match separators (longest first).
		matched := false
		for _, sep := range shellSeparators {
			if strings.HasPrefix(cmd[i:], sep) {
				flush()
				i += len(sep)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	return segs
}

// ExtractCommandBinary returns the leading binary name of one
// segment, with two normalizations:
//
//  1. `KEY=value` env-var assignments at the start are skipped --
//     `FOO=bar baz qux` returns `baz`.
//  2. The result is reduced to its basename so `/usr/bin/python3`,
//     `./script.sh`, and `python3` all return the bare binary name
//     -- consistent with shell PATH resolution and with the
//     workbench-allowlist semantics that care WHICH binary runs,
//     not where on disk it lives.
//
// Returns "" when the segment is empty, only contains assignments,
// or starts with a quote (we don't unwrap quotes -- a quoted path
// is matched as-is by the caller's rule and either passes the
// allowlist or escalates).
func ExtractCommandBinary(seg string) string {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return ""
	}
	for {
		first := firstToken(seg)
		if first == "" {
			return ""
		}
		// Skip a `KEY=value` env-var assignment prefix. The
		// assignment must not contain shell metachars to count.
		if isEnvAssignment(first) {
			rest := strings.TrimLeft(seg[len(first):], " \t")
			if rest == "" {
				return ""
			}
			seg = rest
			continue
		}
		return filepath.Base(first)
	}
}

// CommandBinaries combines SegmentCommand and ExtractCommandBinary --
// returns the leading binary name of every non-empty segment. A
// command that parses to zero binaries returns nil.
func CommandBinaries(cmd string) []string {
	segs := SegmentCommand(cmd)
	if len(segs) == 0 {
		return nil
	}
	var out []string
	for _, s := range segs {
		if b := ExtractCommandBinary(s); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// ContainsSubshell reports whether cmd contains shell metaprogramming
// that the simple segment-based rules can't safely decode:
//
//   - `$( ... )` command substitution
//   - “ ` ... ` “ backtick command substitution
//   - `<<` here-doc / `<<<` here-string redirects
//
// Detection is conservative: a single `$(` outside of quotes is
// enough. Rules that depend on accurate segmentation (notably the
// workbench-allowlist rule, which checks the leading binary) treat
// `true` as "cannot verify -- deny" rather than letting subshells
// smuggle disallowed binaries past the outer-only check.
func ContainsSubshell(cmd string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if !inDouble && c == '\'' {
			inSingle = !inSingle
			continue
		}
		if !inSingle && c == '"' {
			inDouble = !inDouble
			continue
		}
		if inSingle {
			// Inside single quotes nothing interpolates.
			continue
		}
		// Inside double quotes, `$(`/backticks DO interpolate, so
		// they still count.
		if c == '`' {
			return true
		}
		if c == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
			return true
		}
		if !inDouble && c == '<' && i+1 < len(cmd) && cmd[i+1] == '<' {
			// `<<` here-doc (also matches `<<<` here-string).
			return true
		}
	}
	return false
}

// firstToken returns cmd's first whitespace-delimited token,
// stopping at any ASCII whitespace. Does NOT unwrap quotes.
func firstToken(cmd string) string {
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			return cmd[:i]
		}
	}
	return cmd
}

// isEnvAssignment reports whether token looks like a shell env-var
// assignment `KEY=value`: starts with an identifier-y char, contains
// `=` after at least one valid identifier char, and the name half
// has no shell metachars. Conservative -- a token that's not clearly
// an assignment is treated as the binary.
func isEnvAssignment(token string) bool {
	eq := strings.IndexByte(token, '=')
	if eq <= 0 {
		return false
	}
	name := token[:eq]
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9' && i > 0) || c == '_') {
			return false
		}
	}
	return true
}
