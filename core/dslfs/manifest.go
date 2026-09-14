package dslfs

// manifest.go -- the language line a DSL tree declares (epic memql#5356,
// task memql#5357; D4 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A tree says which language it is written in with a two-line file beside its
// .memql files:
//
//	memql = "1.0"
//	edition = "2026"
//
// WHERE IT LIVES. Every domain directory a bundle or a package delivers carries
// its own memql.toml, because a domain directory is the only thing that reaches
// a node: the bundle image copies domain directories into MEMQL_DSL_PATH, the
// package stager stages each domain's own tree, and every mount skips the files
// at its root. A declaration at a bundle's root would never arrive. The
// embedded tree is the one exception, and not really one: it is compiled into
// the engine as a single tree, so it declares its line once, at dsl/memql.toml.
//
// THE FORMAT is a strict subset of TOML, so the file reads the way an author
// expects and a TOML-aware editor highlights it -- but this package reads
// exactly that subset and nothing else, with the standard library alone: blank
// lines, comments from `#` to the end of the line, and `key = "value"` lines
// for the two keys. Anything else is refused with its line number: a third
// key, a table, an unquoted value, an escape, a key written twice. A manifest
// the engine half-reads is a manifest two readers can disagree about, and this
// file decides how every other file in its directory is read.

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// ManifestFile is the name of the file a tree's language line lives in.
const ManifestFile = "memql.toml"

// Manifest is one tree's declared language line. An empty field means the file
// did not declare it; deciding whether that is acceptable, and whether a
// declared value is one the engine speaks, is the loader's job
// (component/memql), because only the loader knows the engine's own line.
type Manifest struct {
	// Language is the `memql = "<major>.<minor>"` line.
	Language string
	// Edition is the `edition = "<year>"` line.
	Edition string
}

// Render is the canonical text of m: what the migrator writes into a domain
// and what a refusal tells an author to add. Two lines, in a fixed order.
func (m Manifest) Render() string {
	return fmt.Sprintf("memql = %q\nedition = %q\n", m.Language, m.Edition)
}

// ManifestError is a refusal to read a manifest.
type ManifestError struct {
	// Line is the 1-based line the refusal is about.
	Line int
	// Msg says what is wrong and what to write instead.
	Msg string
}

func (e *ManifestError) Error() string {
	return fmt.Sprintf("%s line %d: %s", ManifestFile, e.Line, e.Msg)
}

// manifestKeys is the closed key set, in the order Render writes them.
var manifestKeys = []string{"memql", "edition"}

// manifestLine is one `key = "value"` line with an optional trailing comment.
// A value is plain text: no quote and no backslash inside it, because a
// language version or an edition never needs one, and an escape rule is one
// more thing two readers could implement differently.
var manifestLine = regexp.MustCompile(`^([A-Za-z0-9_-]+)[ \t]*=[ \t]*"([^"\\]*)"[ \t]*(?:#.*)?$`)

// ParseManifest reads a memql.toml.
//
// It checks the file's shape and nothing else. A file with a missing key
// parses, with that field empty, so the loader can say which line to add.
func ParseManifest(data []byte) (Manifest, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // an editor's byte-order mark
	var m Manifest
	declaredOn := map[string]int{}
	for i, raw := range strings.Split(string(data), "\n") {
		n := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return Manifest{}, &ManifestError{Line: n, Msg: fmt.Sprintf(
				"%q is a table header; a language manifest has no tables, only the lines memql = %q and edition = %q",
				line, "<major>.<minor>", "<year>")}
		}
		sub := manifestLine.FindStringSubmatch(line)
		if sub == nil {
			return Manifest{}, &ManifestError{Line: n, Msg: fmt.Sprintf(
				"%q is not a key = \"value\" line; write each declaration as memql = \"1.0\" or edition = \"2026\", with the value in double quotes",
				line)}
		}
		key, value := sub[1], sub[2]
		if prev, dup := declaredOn[key]; dup {
			return Manifest{}, &ManifestError{Line: n, Msg: fmt.Sprintf(
				"%s is declared twice (lines %d and %d); declare it once", key, prev, n)}
		}
		declaredOn[key] = n
		switch key {
		case "memql":
			m.Language = value
		case "edition":
			m.Edition = value
		default:
			return Manifest{}, &ManifestError{Line: n, Msg: fmt.Sprintf(
				"%q is not a key a language manifest declares; the keys are %s", key, strings.Join(manifestKeys, " and "))}
		}
	}
	return m, nil
}
