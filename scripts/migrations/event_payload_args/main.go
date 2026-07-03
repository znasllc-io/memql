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
// rewritten. String literals and comments are protected.
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
	// Walk automation headers back-to-front so offsets stay valid as we edit.
	locs := automationHeader.FindAllStringSubmatchIndex(src, -1)
	for i := len(locs) - 1; i >= 0; i-- {
		loc := locs[i]
		openIdx := strings.Index(src[loc[0]:loc[1]], "{") + loc[0]
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
	if loc := argsHeader.FindStringIndex(block); loc != nil {
		open := strings.Index(block[loc[0]:loc[1]], "{") + loc[0]
		closeIdx := matchingBrace(block, open)
		if closeIdx < 0 {
			return block
		}
		existing := block[open+1 : closeIdx]
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

// mapCodeSegments applies fn to the non-string, non-comment segments.
func mapCodeSegments(s string, fn func(string) string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '"':
			j := i + 1
			for j < len(s) && (s[j] != '"' || s[j-1] == '\\') {
				j++
			}
			if j < len(s) {
				j++
			}
			out.WriteString(s[i:j])
			i = j
		case strings.HasPrefix(s[i:], "//"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			out.WriteString(s[i : i+j])
			i += j
		default:
			j := i
			for j < len(s) && s[j] != '"' && !strings.HasPrefix(s[j:], "//") {
				j++
			}
			out.WriteString(fn(s[i:j]))
			i = j
		}
	}
	return out.String()
}

// scrub blanks strings and comments so field collection never reads prose.
func scrub(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '"':
			j := i + 1
			for j < len(s) && (s[j] != '"' || s[j-1] == '\\') {
				j++
			}
			if j < len(s) {
				j++
			}
			out.WriteString(strings.Repeat(" ", j-i))
			i = j
		case strings.HasPrefix(s[i:], "//"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			out.WriteString(strings.Repeat(" ", j))
			i += j
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

func matchingBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
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
	return -1
}
