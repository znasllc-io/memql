package model

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// WriteJSON serializes the model with stable indentation. Output is
// pretty-printed because the file is meant to be diffable in PRs --
// architectural change should show up in code review the same way
// any other source change does.
func (m *Model) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(m)
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
