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

// WriteJSON serializes the model COMPACT, with a stable element order.
//
// Compact, not pretty-printed, and the reason is measured (memql#2844). The
// artifact is ~40MB pretty / 1,261,197 lines, and GitHub's diff API cannot
// generate a patch for a change that touches all of them:
//
//	Server Error: Sorry, this diff is taking too long to generate.
//	{"resource":"PullRequest","field":"diff","code":"not_available"}
//
// dorny/paths-filter reads that diff to decide which CI lanes run, so the
// `changes` job fails, `ci-required` fails, and the PR is unmergeable. The
// sort had to reorder every line once, so this was not avoidable by deferring;
// `.gitattributes -diff` did not help either, since the server still computes
// the patch. Compact makes it one line: 31.6MB, and every future diff is
// trivially generatable.
//
// What that gives up, stated plainly: the file is no longer readable as a
// diff. It never really was -- nobody reviews 40MB of JSON -- and correctness
// is established by TestArchitectureModelIsCurrent regenerating and comparing
// byte-for-byte, not by reading the artifact. The ORDER still matters and is
// still enforced: it is what makes that byte-for-byte comparison possible at
// all.
//
// The sort is what makes that true (memql#2844). Without it, two runs over an
// IDENTICAL tree on the SAME machine emitted the same 121,601 edges in a
// different sequence -- measured -- because the call-graph pass walks Go maps.
// A pretty-printed file whose line order churns is not diffable and cannot be
// gated, so "regenerate and commit" was not a usable operation: every refresh
// buried the real change under a six-figure diff. Sorting here rather than in
// the extractor means EVERY producer of a Model gets the property, including
// any future pass.
// It does NOT mutate the receiver. An in-place sort would be cheaper, but
// Index (model/index.go) stores &m.Nodes[i] / &m.Edges[i] and documents that
// "m must not be mutated for the lifetime of the Index" -- reordering the
// backing arrays under it would leave every pointer in byID / children /
// outgoing / incoming aliasing a DIFFERENT element, so ix.Node(id) would return
// an unrelated node. It would also make WriteJSON unsafe to call concurrently
// on a shared *Model. Latent in this repo (NewIndex has no non-test callers and
// WriteJSON has one), but model is an exported package the out-of-repo cockpit
// imports, and a serializer with a hidden write is a bad contract regardless.
// Copying ~140k elements is noise next to a 42MB encode.
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

// sortForStableOutput orders nodes and edges by a total key.
//
// Edges are a deliberate MULTI-SET -- the doc on Edge says duplicates are
// allowed when they carry different attributes, e.g. two call sites on one
// call-graph edge -- so (From, To, Kind) is not unique and sorting on it alone
// leaves ties in map order. The serialized Attrs are folded in as the final
// tiebreak, which makes the order total: two edges equal on all four fields are
// indistinguishable in the output anyway.
// IsSortedForStableOutput reports whether Nodes and Edges are already in the
// order WriteJSON emits.
//
// Exported so a test can assert the COMMITTED artifact is sorted without
// re-implementing the comparator -- one comparator, two callers (memql#2844).
// The subset drift gate cannot see order at all, and byte-for-byte was the only
// thing enforcing it before; a shuffled model passed.
func (m *Model) IsSortedForStableOutput() bool {
	return sort.SliceIsSorted(m.Nodes, m.nodeLess) && sort.SliceIsSorted(m.Edges, m.edgeLess)
}

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
	{
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
}

func (m *Model) edgeLess(i, j int) bool {
	{
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
