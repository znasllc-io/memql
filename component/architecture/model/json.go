package model

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CanonicalFilename is the default generated topology filename.
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

// WriteJSON serializes the model compactly in stable order. The generated
// output is not committed. Equal edge records are retained: Edges is a multiset.
func (m *Model) WriteJSON(w io.Writer) error {
	out := &Model{
		SchemaVersion: m.SchemaVersion,
		GeneratedAt:   m.GeneratedAt,
		Workspace:     m.Workspace,
		Nodes:         append([]Node(nil), m.Nodes...),
		Edges:         append([]Edge(nil), m.Edges...),
	}
	out.sortForStableOutput()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

// IsSortedForStableOutput reports whether Nodes and Edges are already in the
// order WriteJSON emits.
//
// Shared with consumers and generated-output tests.
func (m *Model) IsSortedForStableOutput() bool {
	return sort.SliceIsSorted(m.Nodes, m.nodeLess) && sort.SliceIsSorted(m.Edges, m.edgeLess)
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
	// Total, like the edge key below. (ID, Kind) alone left two nodes equal on
	// both but differing in Doc/Source/Attrs in unstable sort.Slice order. No
	// such pair exists in the real model today -- 0 duplicate IDs, 0 duplicate
	// (ID, Kind) -- but an undefended asymmetry between the two keys is how the
	// next one gets in.
	sort.Slice(m.Nodes, m.nodeLess)
	sort.Slice(m.Edges, m.edgeLess)
}

func (m *Model) nodeLess(i, j int) bool {
	a, b := m.Nodes[i], m.Nodes[j]
	switch {
	case a.ID != b.ID:
		return a.ID < b.ID
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	case a.Name != b.Name:
		return a.Name < b.Name
	case a.Parent != b.Parent:
		return a.Parent < b.Parent
	case a.Doc != b.Doc:
		return a.Doc < b.Doc
	case sourceKey(a.Source) != sourceKey(b.Source):
		return sourceKey(a.Source) < sourceKey(b.Source)
	}
	return attrsKey(a.Attrs) < attrsKey(b.Attrs)
}

func (m *Model) edgeLess(i, j int) bool {
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
}

// sourceKey renders a SourceRef deterministically for tiebreaking.
func sourceKey(s *SourceRef) string {
	if s == nil {
		return ""
	}
	return s.File + "\x00" + strconv.Itoa(s.Line)
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
