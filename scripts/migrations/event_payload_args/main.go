// Command event_payload_args is the G5 (memql#2367 / event-payload-binding
// ADR Decision 6) tree migrator: for every FULL-FORM event-triggered
// automation it
//
//  1. collects the `event.payload.<field>` reads in the automation block,
//  2. synthesizes (or extends) the `args { }` block with those fields --
//     typed `any`, optional, so the G1 fire-time validation can never refuse
//     an event the pre-migration automation would have accepted (semantics
//     preserved exactly; authors tighten types by hand afterwards),
//  3. rewrites `event.payload.<field>...` to the bare `<field>...` read
//     (G2 resolution), and
//  4. puns call args whose key and value are the same bare identifier
//     (`name: name` -> `name`, G3).
//
// Deterministic + idempotent (a second run is a no-op). Terse automations
// (`automation N @trigger(...) => logic X`) are untouched per the ADR; logic
// bodies (`args.event.payload.X`) are a different surface and are never
// rewritten.
//
// PROTECTION IS WHOLE-FILE, and every scan derives it from ONE segmenter.
// String literals, `//` line comments and `/* */` block comments are protected
// on the same rules the G5 gate itself scans by -- see scanSegments. The scans
// that look INSIDE a block (collectPayloadFields, rewriteBlock) run on it, and
// so do the scans that FIND the blocks: automationHeader and argsHeader match
// against scrub's blanked copy, matchingBrace counts depth over code segments
// only (memql#3193). So a commented-out automation is not an automation, and a
// brace inside a string literal neither opens nor closes a block.
//
// The two ways that used to go wrong are what the outer scans are now tested
// for: a `/* */`-parked automation was rewritten, args block and all, and a `}`
// in a literal mis-terminated the block, silently leaving every read past it
// un-migrated -- and because a truncated block then collects no fields, a
// SECOND run reported the file as clean.
//
// Usage: go run ./scripts/migrations/event_payload_args [-write] <dsl-root>...
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	write = flag.Bool("write", false, "rewrite files in place (default: dry-run report)")

	automationHeader = regexp.MustCompile(`(?m)^(\s*)automation\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
	payloadRead      = regexp.MustCompile(`\bevent\.payload\.([A-Za-z_][A-Za-z0-9_]*)`)
	argsHeader       = regexp.MustCompile(`(?m)^\s*args\s*\{`)
	punPattern       = regexp.MustCompile(`([(,]\s*)([A-Za-z_][A-Za-z0-9_]*):\s*([A-Za-z_][A-Za-z0-9_]*)(\s*[,)])`)
)

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: event_payload_args [-write] <dsl-root>...")
		os.Exit(2)
	}
	changed := 0
	for _, root := range flag.Args() {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "automations.memql") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out, report := migrateFile(string(data))
			if out != string(data) {
				changed++
				fmt.Printf("%s:\n%s", path, report)
				if *write {
					if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "walk %s: %v\n", root, err)
			os.Exit(1)
		}
	}
	if !*write && changed > 0 {
		fmt.Printf("\n%d file(s) would change; re-run with -write\n", changed)
	}
}

// migrateFile rewrites every full-form automation block in the source.
func migrateFile(src string) (string, string) {
	var report strings.Builder
	// Header matching runs against the blanked copy, so an automation parked in
	// a `/* */` comment is not a header. scrub is length- and line-preserving,
	// so the offsets index src unchanged, and every span a header match reports
	// is code -- identical in both views.
	//
	// Walk automation headers back-to-front so offsets stay valid as we edit.
	locs := automationHeader.FindAllStringSubmatchIndex(scrub(src), -1)
	for i := len(locs) - 1; i >= 0; i-- {
		loc := locs[i]
		// Both header patterns END in `\{`, which is the only anchor that
		// survives blanking: prose scrubs to whitespace, so the leading `^\s*`
		// can start a match at the top of a blanked comment and a search for
		// the FIRST `{` in the match span would land on a brace inside it.
		openIdx := loc[1] - 1
		closeIdx := matchingBrace(src, openIdx)
		if closeIdx < 0 {
			continue
		}
		name := src[loc[4]:loc[5]]
		block := src[openIdx+1 : closeIdx]

		fields := collectPayloadFields(block)
		if len(fields) == 0 {
			continue
		}
		newBlock := rewriteBlock(block, fields)
		newBlock = ensureArgs(newBlock, fields)
		src = src[:openIdx+1] + newBlock + src[closeIdx:]
		fmt.Fprintf(&report, "  automation %s: args += %v\n", name, fields)
	}
	return src, report.String()
}

// collectPayloadFields returns the sorted set of event.payload head fields
// referenced outside strings/comments.
func collectPayloadFields(block string) []string {
	scrubbed := scrub(block)
	set := map[string]bool{}
	for _, m := range payloadRead.FindAllStringSubmatch(scrubbed, -1) {
		set[m[1]] = true
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// rewriteBlock replaces event.payload.<f> with the bare read and applies
// punning, respecting strings/comments via segment-wise processing.
func rewriteBlock(block string, fields []string) string {
	inSet := map[string]bool{}
	for _, f := range fields {
		inSet[f] = true
	}
	return mapCodeSegments(block, func(code string) string {
		code = payloadRead.ReplaceAllStringFunc(code, func(m string) string {
			f := payloadRead.FindStringSubmatch(m)[1]
			if inSet[f] {
				return f
			}
			return m
		})
		// Punning: k: k -> k (only for identical bare identifiers).
		code = punPattern.ReplaceAllStringFunc(code, func(m string) string {
			parts := punPattern.FindStringSubmatch(m)
			if parts[2] == parts[3] {
				return parts[1] + parts[2] + parts[4]
			}
			return m
		})
		return code
	})
}

// ensureArgs inserts (or extends) the args block at the top of the
// automation body with `<field> any` entries for missing fields.
func ensureArgs(block string, fields []string) string {
	// Same blanked view as the automation header scan: a commented-out args
	// block is not an args block, so extending it (and leaving the automation
	// with no live args at all) is not reachable.
	if loc := argsHeader.FindStringIndex(scrub(block)); loc != nil {
		open := loc[1] - 1 // the pattern's trailing `\{`; see migrateFile
		closeIdx := matchingBrace(block, open)
		if closeIdx < 0 {
			return block
		}
		// The already-present probe reads the blanked interior too -- a field
		// named only in a comment is not present, and skipping it would leave
		// the read the G1 fire-time validation then refuses.
		existing := scrub(block[open+1 : closeIdx])
		var add strings.Builder
		for _, f := range fields {
			if !regexp.MustCompile(`(?m)^\s*` + f + `\s`).MatchString(existing) {
				fmt.Fprintf(&add, "    %s any\n", f)
			}
		}
		return block[:closeIdx] + add.String() + block[closeIdx:]
	}
	var b strings.Builder
	b.WriteString("\n  args {\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "    %s any\n", f)
	}
	b.WriteString("  }\n")
	return b.String() + block
}

// segment is a half-open byte range [start, end) of the source, tagged with
// whether it is code (outside every literal and comment) or prose.
type segment struct {
	start, end int
	code       bool
}

// scanSegments splits s into alternating code and prose runs: string
// literals, `//` line comments and `/* */` block comments are prose,
// everything else is code. The segments tile s exactly, in ascending order.
//
// ONE scanner, TWO consumers (memql#3045). This file used to carry two
// verbatim copies of the scan -- one in mapCodeSegments, one in scrub -- and
// both carried the string-scanning bug memql#2949 fixed in the G5 gate they
// remediate: `(s[j] != '"' || s[j-1] == '\\')` cannot tell an escaped quote
// from a quote that follows a COMPLETED `\\` escape, so a literal ending in a
// backslash pair read its own closing quote as escaped and consumed on to the
// next one. In scrub that blanks real code, dropping fields from the args
// block; in mapCodeSegments it is worse, because that function decides what
// gets REWRITTEN -- over-consuming makes real code look like literal interior
// and silently skips it, in a tool documented to be run with -write.
//
// Two copies of a scan is also two chances to disagree with each other. One
// scanner cannot, which is why the fix is a shared segmenter rather than the
// same patch applied twice.
//
// NOT delegated to an existing in-tree scrubber. parser.BlankCommentsAndStrings
// and baseparser.BlankComments both return a BLANKED COPY, which serves scrub
// but not mapCodeSegments: that one must pass prose through VERBATIM into the
// rewritten output, and prose spans cannot be recovered from a blanked copy --
// a blank line inside a block comment is byte-identical in both views, the
// trap baseparser.CommentSpans' own doc records. Deriving both consumers from
// spans is what removes the copy; importing a blanker would have left one.
//
// The arms mirror scrubSourceForPayloadScan in component/automations
// (args_resolution.go), because a remediation tool that disagrees with the
// gate it remediates is the defect in this issue:
//   - escape state is TRACKED, never inferred from the preceding byte;
//   - a literal MAY span lines (the lexer's scanString has no newline case);
//   - an UNBALANCED literal costs only its own line, not the rest of the file;
//   - `/*` is prose (memql#2872), so an ordinary note mentioning
//     `event.payload.status` above an automation is neither collected as a
//     field nor rewritten.
//
// Backticks are not an arm, for the same reason the gate has none: this walks
// authored `automations.memql`, where the backtick-wrapped form is a rewriter
// emission and never appears.
func scanSegments(s string) []segment {
	var out []segment
	add := func(start, end int, code bool) {
		if start >= end {
			return
		}
		if n := len(out); n > 0 && out[n-1].code == code && out[n-1].end == start {
			out[n-1].end = end
			return
		}
		out = append(out, segment{start: start, end: end, code: code})
	}
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '"':
			j := i + 1
			escaped := false
			closed := false
			firstNL := -1
			for j < len(s) {
				c := s[j]
				if c == '\n' && firstNL < 0 {
					firstNL = j
				}
				j++
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' {
					escaped = true
					continue
				}
				if c == '"' {
					closed = true
					break // closing quote, consumed above
				}
			}
			if !closed && firstNL >= 0 {
				j = firstNL // unbalanced: stop at the newline
			}
			add(i, j, false)
			i = j
		case strings.HasPrefix(s[i:], "//"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			add(i, i+j, false)
			i += j
		case strings.HasPrefix(s[i:], "/*"):
			// An unterminated block comment runs to EOF, matching the lexer.
			j := len(s) - i
			if end := strings.Index(s[i+2:], "*/"); end >= 0 {
				j = end + 4
			}
			add(i, i+j, false)
			i += j
		default:
			j := i
			for j < len(s) && s[j] != '"' &&
				!strings.HasPrefix(s[j:], "//") && !strings.HasPrefix(s[j:], "/*") {
				j++
			}
			add(i, j, true)
			i = j
		}
	}
	return out
}

// mapCodeSegments applies fn to the code segments and passes every string
// literal and comment through verbatim.
func mapCodeSegments(s string, fn func(string) string) string {
	var out strings.Builder
	out.Grow(len(s))
	for _, seg := range scanSegments(s) {
		if seg.code {
			out.WriteString(fn(s[seg.start:seg.end]))
			continue
		}
		out.WriteString(s[seg.start:seg.end])
	}
	return out.String()
}

// scrub blanks strings and comments so field collection never reads prose.
// Newlines inside a blanked span are preserved, so the result has the same
// length AND the same line numbering as the input.
func scrub(s string) string {
	out := []byte(s)
	for _, seg := range scanSegments(s) {
		if seg.code {
			continue
		}
		for k := seg.start; k < seg.end; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
	}
	return string(out)
}

// matchingBrace returns the offset of the `}` closing the `{` at open, or -1
// if the block is unterminated. The depth walk visits the CODE segments of
// scanSegments only (memql#3193), so a brace inside a string literal or a
// comment neither opens nor closes a block. A bare depth counter ended
// `automation a { step s { label: "close } brace" } step t { ... } }` at the
// literal's brace, putting `step t` outside the span the migration ever reads:
// its payload reads reached neither the args block nor the rewrite, and the
// truncated block collecting no fields made the next run report the file clean.
func matchingBrace(s string, open int) int {
	depth := 0
	for _, seg := range scanSegments(s) {
		if !seg.code || seg.end <= open {
			continue
		}
		start := seg.start
		if start < open {
			start = open
		}
		for i := start; i < seg.end; i++ {
			switch s[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}
