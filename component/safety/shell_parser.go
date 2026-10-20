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

// SegmentCommand splits cmd at shell separators outside quotes, escapes and
// comments. It preserves the original segment text; it does not evaluate shell
// expansions. Call ContainsSubshell before trusting segment-level allowlists.
func SegmentCommand(cmd string) []string {
	segments, _ := scanShellCommand(cmd)
	return segments
}

// scanShellCommand shares quote/escape/comment handling between segmentation
// and detection of constructs that cannot be verified by the binary allowlist.
func scanShellCommand(cmd string) ([]string, bool) {
	var segments []string
	var cur strings.Builder
	flush := func() {
		if segment := strings.TrimSpace(cur.String()); segment != "" {
			segments = append(segments, segment)
		}
		cur.Reset()
	}
	inSingle, inDouble, wordStart, complex := false, false, true, false
	for i := 0; i < len(cmd); {
		c := cmd[i]
		// Backslash is literal in single quotes. Elsewhere it protects the
		// following quote/separator; preserve both bytes for the real shell.
		if !inSingle && c == '\\' && i+1 < len(cmd) {
			cur.WriteString(cmd[i : i+2])
			if cmd[i+1] != '\n' {
				wordStart = false
			}
			i += 2
			continue
		}
		// A quote in a shell comment must not hide commands on later lines.
		if !inSingle && !inDouble && wordStart && c == '#' {
			end := i
			for end < len(cmd) && cmd[end] != '\n' {
				end++
			}
			cur.WriteString(cmd[i:end])
			i = end
			continue
		}
		if !inDouble && c == '\'' {
			inSingle = !inSingle
			wordStart = false
			cur.WriteByte(c)
			i++
			continue
		}
		if !inSingle && c == '"' {
			inDouble = !inDouble
			wordStart = false
			cur.WriteByte(c)
			i++
			continue
		}
		if !inSingle && (c == '`' || c == '$' && i+1 < len(cmd) && cmd[i+1] == '(' ||
			!inDouble && c == '<' && i+1 < len(cmd) && cmd[i+1] == '<') {
			complex = true
		}
		if !inSingle && !inDouble {
			matched := false
			for _, sep := range shellSeparators {
				if strings.HasPrefix(cmd[i:], sep) {
					flush()
					i += len(sep)
					wordStart = true
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			wordStart = c == ' ' || c == '\t'
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	return segments, complex
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

// ContainsSubshell reports command substitutions and heredocs that the simple
// segment-based rules cannot verify. Single-quoted and escaped literals do not
// execute; substitutions inside double quotes do. Comments are not executed.
func ContainsSubshell(cmd string) bool {
	_, complex := scanShellCommand(cmd)
	return complex
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
