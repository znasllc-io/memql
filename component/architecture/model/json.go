package model

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// CanonicalFilename is the default on-disk name the extractor writes
// and the cockpit reads. Kept at workspace root so both repos (the
// one being analyzed and the cockpit binary) can find it via a single
// relative path.
const CanonicalFilename = "topology.model.json"

// NewModel constructs an empty Model stamped with the current schema
// version and generation time. Extractors should always go through
// this constructor so future schema changes have a single rollover
// point.
func NewModel(workspace string) *Model {
	return &Model{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Workspace:     workspace,
		Nodes:         []Node{},
		Edges:         []Edge{},
	}
}

// WriteJSON serializes the model with stable indentation AND a stable
// element order. Output is pretty-printed because the file is meant to be
// diffable in PRs -- architectural change should show up in code review the
// same way any other source change does.
//
// The sort is what makes that true (memql#2844). Without it, two runs over an
// IDENTICAL tree on the SAME machine emitted the same 121,601 edges in a
// different sequence -- measured -- because the call-graph pass walks Go maps.
// A pretty-printed file whose line order churns is not diffable and cannot be
// gated, so "regenerate and commit" was not a usable operation: every refresh
// buried the real change under a six-figure diff. Sorting here rather than in
// the extractor means EVERY producer of a Model gets the property, including
// any future pass.
func (m *Model) WriteJSON(w io.Writer) error {
	m.sortForStableOutput()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(m)
}

// sortForStableOutput orders nodes and edges by a total key.
//
// Edges are a deliberate MULTI-SET -- the doc on Edge says duplicates are
// allowed when they carry different attributes, e.g. two call sites on one
// call-graph edge -- so (From, To, Kind) is not unique and sorting on it alone
// leaves ties in map order. The serialized Attrs are folded in as the final
// tiebreak, which makes the order total: two edges equal on all four fields are
// indistinguishable in the output anyway.
func (m *Model) sortForStableOutput() {
	sort.Slice(m.Nodes, func(i, j int) bool {
		if m.Nodes[i].ID != m.Nodes[j].ID {
			return m.Nodes[i].ID < m.Nodes[j].ID
		}
		return m.Nodes[i].Kind < m.Nodes[j].Kind
	})
	sort.Slice(m.Edges, func(i, j int) bool {
		a, b := m.Edges[i], m.Edges[j]
		switch {
		case a.From != b.From:
			return a.From < b.From
		case a.To != b.To:
			return a.To < b.To
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		}
		return attrsKey(a.Attrs) < attrsKey(b.Attrs)
	})
}

// attrsKey renders an attrs map as a deterministic string for tiebreaking.
func attrsKey(attrs map[string]string) string {
	if len(attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(attrs[k])
		b.WriteByte('\x00')
	}
	return b.String()
}

// WriteFile writes the model to path using WriteJSON. Truncates any
// existing file.
func (m *Model) WriteFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := m.WriteJSON(f); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return nil
}

// ReadFile loads a model from disk and validates its schema version
// against the one this binary was built with. Mismatches are an
// error rather than a best-effort migration: cockpit and extractor
// always ship together, so a divergent version means the developer
// forgot to regenerate.
func ReadFile(path string) (*Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return ReadJSON(f)
}

// ReadJSON decodes a model from r and validates schema version.
func ReadJSON(r io.Reader) (*Model, error) {
	var m Model
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields() // strict on unknown top-level keys to surface schema drift early
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode model: %w", err)
	}
	if m.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("schema version mismatch: file=%q binary=%q -- regenerate the model with memql-arch", m.SchemaVersion, SchemaVersion)
	}
	return &m, nil
}
