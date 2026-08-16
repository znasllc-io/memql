// Package genesis owns the .env -> genesis.znas ENVELOPE: it reads a
// developer's .env, validates it against memql's manifest, and seals the
// result into an encrypted blob under MEMQL_MASTER_KEY that a node autoloads
// into its own environment at boot.
//
// THE REGISTRY HALF HAS MOVED (memql#3963). The manifest, boot-time
// validation, domain derivations, the legacy-alias shim and the repo-root
// `.env` override now live in component/envregistry, which is the half that
// survives. This package is the envelope and nothing else, and it is being
// deleted outright (memql#3966): once config has one delivery path -- the
// memql-secrets Secret -- the sealing CLI, the decrypt tool, the autoload
// hook, the eleven-Deployment kustomize patch and the .znas file format all
// stop earning their keep.
//
// Do not add to this package. Anything durable belongs in envregistry.
package genesis

import (
	"strings"

	"github.com/znasllc-io/memql/component/envregistry"
)

// EnvEntry is re-exported from envregistry so this package's own surface
// stays readable while it lives out its remaining days. The type is the
// registry's -- there is exactly one EnvEntry in the tree.
type EnvEntry = envregistry.EnvEntry

// ParseEnvFile is re-exported from envregistry for the same reason.
var ParseEnvFile = envregistry.ParseEnvFile

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
