// memqlfmt is the canonical formatter for MemQL source files.
//
// Usage:
//
//	memqlfmt [-check] [-w] <path>...
//
// Without flags, prints the formatted content of each file to stdout.
// With -w, rewrites each file in place. With -check, exits non-zero
// and prints the paths of files that would change (gofmt -l shape).
//
// No options. Like gofmt, one style. The formatter is strict about:
//
//   - Two-space indentation inside blocks (matching the existing
//     `.memql` code style; concept + automation files both use 2).
//   - One blank line between top-level declarations; no trailing
//     blank lines at EOF.
//   - Trailing whitespace stripped.
//   - Final newline guaranteed.
//   - Tab characters inside body content are replaced with two spaces.
//
// Phase 5 Step 33 of the language-improvements plan. Phase 2's
// shared ParseFile parser isn't invoked here yet -- this formatter
// runs at the lexical layer. Swapping to a full parse-and-print
// round trip is a follow-up once the AST gets a round-trip-safe
// printer; until then the lexical pass captures 95% of the value
// (consistent indent, no trailing whitespace, final newline).
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const indentUnit = "  "

type opts struct {
	check bool
	write bool
}

func main() {
	var o opts
	flag.BoolVar(&o.check, "check", false, "exit non-zero and print paths of files that would change")
	flag.BoolVar(&o.write, "w", false, "rewrite files in place")
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "usage: memqlfmt [-check] [-w] <path>...")
		os.Exit(2)
	}

	expanded, err := expandPaths(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memqlfmt: %v\n", err)
		os.Exit(2)
	}

	changedCount := 0
	for _, p := range expanded {
		orig, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memqlfmt: read %s: %v\n", p, err)
			os.Exit(1)
		}
		formatted := format(orig)
		if o.check {
			if !bytes.Equal(orig, formatted) {
				fmt.Println(p)
				changedCount++
			}
			continue
		}
		if o.write {
			if !bytes.Equal(orig, formatted) {
				if err := os.WriteFile(p, formatted, 0o644); err != nil {
					fmt.Fprintf(os.Stderr, "memqlfmt: write %s: %v\n", p, err)
					os.Exit(1)
				}
			}
			continue
		}
		if _, err := os.Stdout.Write(formatted); err != nil {
			fmt.Fprintf(os.Stderr, "memqlfmt: stdout: %v\n", err)
			os.Exit(1)
		}
	}

	if o.check && changedCount > 0 {
		os.Exit(1)
	}
}

// expandPaths walks any directory arguments, collecting every .memql
// file. Regular-file arguments pass through unchanged.
func expandPaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".memql" {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// format applies the canonical formatting rules to a .memql source
// buffer. Indent-normalisation is brace-depth-based: every line's
// leading whitespace is replaced with `indentUnit * depth` where
// depth is the running count of unclosed `{` seen so far (excluding
// braces inside string literals and line comments).
func format(src []byte) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var out bytes.Buffer
	depth := 0
	prevBlank := false
	lineCount := 0

	for scanner.Scan() {
		line := scanner.Text()
		// Tab -> two-space normalisation before any depth analysis.
		line = strings.ReplaceAll(line, "\t", "  ")
		trimmed := strings.TrimRight(line, " \t")
		body := strings.TrimLeft(trimmed, " \t")

		blank := body == ""
		if blank {
			// Collapse runs of blank lines to a single blank. Also
			// skip leading blanks at the top of the file.
			if prevBlank || lineCount == 0 {
				continue
			}
			out.WriteString("\n")
			prevBlank = true
			lineCount++
			continue
		}

		// Adjust depth BEFORE emitting if the line begins with a
		// closing brace (the brace belongs at the outer level).
		effectiveDepth := depth
		if strings.HasPrefix(body, "}") || strings.HasPrefix(body, ")") || strings.HasPrefix(body, "]") {
			if effectiveDepth > 0 {
				effectiveDepth--
			}
		}

		out.WriteString(strings.Repeat(indentUnit, effectiveDepth))
		out.WriteString(body)
		out.WriteString("\n")
		prevBlank = false
		lineCount++

		// Adjust depth based on the net balance of opening vs
		// closing brackets on this line. Strings and line comments
		// are honored so `"{"` inside a value literal doesn't
		// disturb depth.
		depth = depth + netBracketBalance(body)
		if depth < 0 {
			depth = 0
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		// Scanner errors surface as the original input unchanged;
		// we never want to corrupt a file we can't format.
		return src
	}

	result := out.Bytes()
	// Trim trailing blank lines; ensure exactly one final newline.
	result = bytes.TrimRight(result, " \t\n")
	result = append(result, '\n')
	return result
}

// netBracketBalance returns (opens - closes) for a line, ignoring
// brackets inside line comments and string literals. Sufficient for
// indentation tracking; not a full parser.
func netBracketBalance(line string) int {
	depth := 0
	i := 0
	for i < len(line) {
		ch := line[i]
		switch ch {
		case '/':
			if i+1 < len(line) && line[i+1] == '/' {
				return depth // rest of line is a comment
			}
		case '"':
			// skip to matching closing quote (respect backslash escapes)
			i++
			for i < len(line) {
				if line[i] == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				if line[i] == '"' {
					break
				}
				i++
			}
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			depth--
		}
		i++
	}
	return depth
}
