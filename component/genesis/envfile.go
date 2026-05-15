// Package genesis owns the .env -> genesis.znas orchestration: reads
// a developer's .env, validates it against memql's manifest, and seals
// the result into an encrypted envelope under MEMQL_MASTER_KEY.
//
// The package is shared between memql-cockpit (which drives it from
// the first-launch wizard) and any other binary that wants to create
// / update / decrypt a genesis envelope without depending on a
// running cluster -- notably the identity service, which reads
// genesis.znas in bootstrap setups.
//
// See docs/planning/genesis-implementation-plan.md for the full design.
package genesis

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// EnvEntry is one parsed line: a key, its value, and the original
// source line number (used in validation error messages). Order is
// preserved as the file is read; SerializeEntries re-emits in the
// same order.
type EnvEntry struct {
	Name  string
	Value string
	Line  int
}

// ParseEnvFile reads a .env file from disk. Skips comments and blank
// lines; tolerates an optional `export ` prefix; strips matching
// single or double quotes around the value. Returns a typed error
// for malformed input (missing `=`, empty key).
func ParseEnvFile(path string) ([]EnvEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("envfile: open %s: %w", path, err)
	}
	defer f.Close()
	return parseEnv(f, path)
}

func parseEnv(r io.Reader, label string) ([]EnvEntry, error) {
	var out []EnvEntry
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("envfile: %s:%d: line lacks '=': %q", label, lineNo, line)
		}
		name := strings.TrimSpace(line[:eq])
		value := line[eq+1:]
		if name == "" {
			return nil, fmt.Errorf("envfile: %s:%d: empty key", label, lineNo)
		}
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		out = append(out, EnvEntry{Name: name, Value: value, Line: lineNo})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("envfile: %s: scan: %w", label, err)
	}
	return out, nil
}

// SerializeEntries renders entries in the same shape ParseEnvFile
// expects to read back: one `KEY=VALUE\n` per entry, in input order.
// Output is what gets sealed into the genesis envelope.
func SerializeEntries(entries []EnvEntry) []byte {
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.Name)
		sb.WriteByte('=')
		sb.WriteString(e.Value)
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

// RewriteMasterKeyAssignment rewrites raw so the first uncommented
// MEMQL_MASTER_KEY assignment carries newValue, preserving any
// leading whitespace and optional `export ` prefix. When no
// uncommented assignment exists, a fresh `MEMQL_MASTER_KEY=<value>`
// line is appended. Returns the rewritten bytes and the action
// taken via replaced / appended bools. Comments and blank lines are
// preserved verbatim.
//
// Only the FIRST matching assignment is rewritten; duplicates lower
// in the file are left alone. Multiple assignments are a pre-existing
// authoring smell -- the bash semantics of "last write wins" still
// holds for `set -a; . file` callers, so the operator's intent for a
// duplicated key is already ambiguous before we touch it.
func RewriteMasterKeyAssignment(raw []byte, newValue string) (out []byte, replaced, appended bool) {
	lines := splitLinesPreservingTerminator(raw)
	for i, line := range lines {
		if !lineAssignsMasterKey(line.content) {
			continue
		}
		lines[i].content = rebuildMasterKeyLine(line.content, newValue)
		replaced = true
		break
	}
	if !replaced {
		needSep := len(raw) > 0 && raw[len(raw)-1] != '\n'
		if needSep {
			lines = append(lines, envLine{content: "", terminator: "\n"})
		}
		lines = append(lines, envLine{content: "MEMQL_MASTER_KEY=" + newValue, terminator: "\n"})
		appended = true
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.content)
		b.WriteString(l.terminator)
	}
	return []byte(b.String()), replaced, appended
}

type envLine struct {
	content    string
	terminator string // "\n" or "" for a file that doesn't end with newline.
}

func splitLinesPreservingTerminator(raw []byte) []envLine {
	if len(raw) == 0 {
		return nil
	}
	s := string(raw)
	var out []envLine
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, envLine{content: s[start:i], terminator: "\n"})
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, envLine{content: s[start:], terminator: ""})
	}
	return out
}

func lineAssignsMasterKey(line string) bool {
	s := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(s, "#") {
		return false
	}
	s = strings.TrimPrefix(s, "export ")
	s = strings.TrimLeft(s, " \t")
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		return false
	}
	return strings.TrimSpace(s[:eq]) == "MEMQL_MASTER_KEY"
}

func rebuildMasterKeyLine(line, newValue string) string {
	trimmed := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(trimmed)]
	prefix := ""
	if strings.HasPrefix(trimmed, "export ") {
		prefix = "export "
		trimmed = strings.TrimPrefix(trimmed, "export ")
		trimmed = strings.TrimLeft(trimmed, " \t")
	}
	_ = trimmed
	return indent + prefix + "MEMQL_MASTER_KEY=" + newValue
}
