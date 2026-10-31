// Package goaleval defines shared real-goal evaluation contracts. It has no
// engine, provider or operator-runner dependency and never solves a goal.
package goaleval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
)

const SchemaVersion = 1

type Catalog struct {
	SchemaVersion int        `json:"schemaVersion"`
	Version       string     `json:"version"`
	Scenarios     []Scenario `json:"scenarios"`
}

type Scenario struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Title   string `json:"title"`
	Domain  string `json:"domain"`
	// PlannedLength is declared before execution, never inferred from elapsed time.
	// Omission preserves fingerprints of the original controlled corpus.
	PlannedLength string      `json:"plannedLength,omitempty"`
	Goal          string      `json:"goal"`
	Fixture       Fixture     `json:"fixture"`
	Constraints   []string    `json:"constraints"`
	Injections    []Injection `json:"injections"`
	Criteria      []Criterion `json:"criteria"`
}

type Fixture struct {
	ID      string            `json:"id"`
	Version string            `json:"version"`
	Kind    string            `json:"kind"`
	Files   map[string]string `json:"files"` // relative path -> SHA256 of exact bytes
}

type Injection struct {
	ID       string `json:"id"`
	Boundary string `json:"boundary"`
	Kind     string `json:"kind"`
	Content  string `json:"content"`
}

type Criterion struct {
	ID       string `json:"id"`
	Claim    string `json:"claim"`
	Oracle   string `json:"oracle"`
	Required bool   `json:"required"`
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var version = regexp.MustCompile(`^[1-9][0-9]*\.[0-9]+\.[0-9]+$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func Decode[T any](raw []byte) (T, error) {
	var v T
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return v, fmt.Errorf("expected exactly one JSON document")
	}
	return v, nil
}

func Load(fsys fs.FS, name string) (Catalog, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Catalog{}, err
	}
	c, err := Decode[Catalog](raw)
	if err != nil {
		return c, err
	}
	if err = c.Validate(); err != nil {
		return c, err
	}
	for _, s := range c.Scenarios {
		if _, err := ReadFixture(fsys, path.Dir(name), s.Fixture); err != nil {
			return c, fmt.Errorf("%s fixture: %w", s.ID, err)
		}
	}
	return c, nil
}

// ReadFixture returns the exact project files to materialize in an isolated
// evaluation directory. Repository fixtures are data, so their go.mod and
// intentionally broken code never enter the platform's module/build graph.
func ReadFixture(fsys fs.FS, catalogDir string, fixture Fixture) (map[string][]byte, error) {
	if !fs.ValidPath(catalogDir) || !identifier.MatchString(fixture.ID) || len(fixture.Files) == 0 {
		return nil, fmt.Errorf("invalid fixture location or file manifest")
	}
	raw, err := fs.ReadFile(fsys, path.Join(catalogDir, "fixtures", fixture.ID+".json"))
	if err != nil {
		return nil, err
	}
	type snapshot struct {
		SchemaVersion int               `json:"schemaVersion"`
		Files         map[string]string `json:"files"`
	}
	saved, err := Decode[snapshot](raw)
	if err != nil {
		return nil, err
	}
	if saved.SchemaVersion != SchemaVersion || len(saved.Files) != len(fixture.Files) {
		return nil, fmt.Errorf("fixture snapshot does not match its file manifest")
	}
	files := make(map[string][]byte, len(saved.Files))
	for name, want := range fixture.Files {
		content, exists := saved.Files[name]
		if !exists || !fs.ValidPath(name) || name == "." || !digest.MatchString(want) {
			return nil, fmt.Errorf("invalid or missing fixture file %q", name)
		}
		b := []byte(content)
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != want {
			return nil, fmt.Errorf("fixture file %s changed without a reviewed digest", name)
		}
		files[name] = b
	}
	return files, nil
}

func (c Catalog) Validate() error {
	if c.SchemaVersion != SchemaVersion || !version.MatchString(c.Version) || len(c.Scenarios) == 0 {
		return fmt.Errorf("goal catalog needs schemaVersion %d, a version and scenarios", SchemaVersion)
	}
	seen := map[string]bool{}
	for _, s := range c.Scenarios {
		if !identifier.MatchString(s.ID) || seen[s.ID] || !version.MatchString(s.Version) || s.Title == "" || s.Domain == "" || s.Goal == "" {
			return fmt.Errorf("invalid or duplicate scenario %q", s.ID)
		}
		seen[s.ID] = true
		if s.PlannedLength != "" && s.PlannedLength != "short" && s.PlannedLength != "medium" && s.PlannedLength != "long" {
			return fmt.Errorf("%s has invalid planned length", s.ID)
		}
		f := s.Fixture
		if !identifier.MatchString(f.ID) || !version.MatchString(f.Version) || (f.Kind != "local" && f.Kind != "github") || len(f.Files) == 0 {
			return fmt.Errorf("%s needs an isolated, versioned fixture", s.ID)
		}
		for file, hash := range f.Files {
			if !fs.ValidPath(file) || file == "." || !digest.MatchString(hash) {
				return fmt.Errorf("%s has invalid fixture file %q", s.ID, file)
			}
		}
		if len(s.Constraints) == 0 || len(s.Criteria) == 0 {
			return fmt.Errorf("%s needs constraints and criteria", s.ID)
		}
		ids := map[string]bool{}
		required := false
		for _, a := range s.Criteria {
			if !identifier.MatchString(a.ID) || ids[a.ID] || a.Claim == "" || a.Oracle == "" {
				return fmt.Errorf("%s has invalid criterion %q", s.ID, a.ID)
			}
			ids[a.ID] = true
			required = required || a.Required
		}
		if !required {
			return fmt.Errorf("%s needs a required criterion", s.ID)
		}
		ids = map[string]bool{}
		for _, a := range s.Injections {
			if !identifier.MatchString(a.ID) || ids[a.ID] || a.Boundary == "" || a.Content == "" {
				return fmt.Errorf("%s has invalid injection %q", s.ID, a.ID)
			}
			ids[a.ID] = true
			switch a.Kind {
			case "tool-unavailable", "test-failure", "checkpoint", "missing-context", "feedback", "approval", "concurrent-change":
			default:
				return fmt.Errorf("%s has unknown injection kind %q", s.ID, a.Kind)
			}
		}
	}
	return nil
}

// Fingerprint ignores formatting, but includes every task and oracle change.
func (s Scenario) Fingerprint() string {
	return fingerprint(s)
}

// JSON object keys are sorted at every level, so the UI and Go verifier can
// bind the same contract without depending on struct declaration order.
func fingerprint(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var normalized any
	if json.Unmarshal(b, &normalized) != nil {
		return ""
	}
	b, _ = json.Marshal(normalized)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
