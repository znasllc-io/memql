package memoryNodes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// declared_metadata_annotations_test.go -- memql#2960.
//
// Four field annotations are DECLARED METADATA: @unique, @immutable, @secret
// and @default parse, reach the emitted schema, and nothing in the engine acts
// on them. The reference (dsl/_reference/_concept.memql section 8),
// docs/public/language/attribute-matrix.md and reserved.md all now say so
// plainly, having previously promised enforcement that does not exist.
//
// A documentation-only fix would repeat the very failure memql#2960 is an
// instance of -- a rule recorded somewhere and enforced nowhere. So the split
// is gated from both sides:
//
//   - the keys are still EMITTED (an author's declaration is not silently
//     dropped, and @pii's history shows a marker can become enforced later);
//   - the keys are still read by NOTHING, which is what makes the docs true.
//
// When enforcement lands for any of them, the second half fails and names the
// documents that have to change in the same commit. That is the point: the
// test is a tripwire on the docs, not an endorsement of the gap.

// buildFixtureConcept parses a single-concept fixture and builds it. The
// concept is picked out of File.Definitions rather than via
// component/memql.ExtractConceptDecls, which would be an import cycle from
// here.
func buildFixtureConcept(t *testing.T, src string) *Concept {
	t.Helper()
	file, err := parser.ParseFile(src)
	if err != nil {
		t.Fatalf("fixture did not parse: %v", err)
	}
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		c, err := BuildConceptFromDecl(decl, "v1:ref:probe")
		if err != nil {
			t.Fatalf("fixture does not build: %v", err)
		}
		return c
	}
	t.Fatal("fixture declared no concept, so it measures nothing")
	return nil
}

// helper: build a concept from source and return its property schemas.
func propertySchemas(t *testing.T, src string) map[string]map[string]any {
	t.Helper()
	c := buildFixtureConcept(t, src)
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc.Properties
}

const declaredMetadataFixture = `@version("1.0.0")
@namespace("ref")
@description("d")
concept probe {
  label      string  @required @description("l")
  uniqueKey  string  @unique @description("u")
  externalId string  @immutable @description("i")
  apiKey     string  @secret @description("s")
  tier       string  @default("bronze") @description("t")
}
`

// The emission half. An author's declaration must survive into the schema --
// dropping it would make the annotation unrecoverable when enforcement lands,
// and @pii is the precedent for a marker graduating from declared to enforced
// without a schema change.
func TestDeclaredMetadataAnnotationsAreStillEmitted(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	for _, tc := range []struct{ field, key string }{
		{"uniqueKey", "x-unique"},
		{"externalId", "x-immutable"},
		{"apiKey", "x-secret"},
		{"tier", "default"},
	} {
		if _, ok := props[tc.field][tc.key]; !ok {
			t.Errorf("%s no longer emits %q. If the annotation was retired rather than left as "+
				"declared metadata, section 8 of dsl/_reference/_concept.memql still lists it "+
				"as accepted -- update it in the same change.\n  got: %v",
				tc.field, tc.key, props[tc.field])
		}
	}
}

// The half that makes the documentation true, and the tripwire on it.
//
// Counted rather than asserted absent, because the emit site is itself an
// occurrence: exactly one means "written, never read". A second occurrence is
// a reader, which is enforcement, which means the docs are now wrong.
func TestDeclaredMetadataKeysAreReadByNothing(t *testing.T) {
	root := repoRoot(t)
	for _, key := range []string{"x-unique", "x-immutable", "x-secret"} {
		t.Run(key, func(t *testing.T) {
			hits := grepNonTestGo(t, root, `"`+key+`"`)
			if len(hits) != 1 {
				t.Errorf("%q now appears in %d non-test Go locations, not 1 (the emit site).\n"+
					"  %s\n\n"+
					"If enforcement landed, that is good news and three documents are now WRONG. "+
					"Update all of them in the same change:\n"+
					"  - dsl/_reference/_concept.memql section 8 (move it out of DECLARED METADATA, "+
					"and fix the @description on its worked example -- that string ships in the schema)\n"+
					"  - docs/public/language/attribute-matrix.md\n"+
					"  - docs/public/language/reserved.md",
					key, len(hits), strings.Join(hits, "\n  "))
			}
		})
	}
}

// @default's gap is different in kind: the key IS emitted into the schema, and
// the schema is a *validation* document, so nothing applies it on insert.
// Pinned separately because the fix is separate -- applying it means touching
// the write path, not adding a reader for an x- key.
//
// This is the behaviour CLAUDE.md already states ("a concept-field @default is
// NOT a substitute -- it is never applied on insert either (memql#2960), so ??
// is the only mechanism"), so if it ever changes, CLAUDE.md changes with it.
func TestDefaultIsEmittedButNeverApplied(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	if got := props["tier"]["default"]; got != "bronze" {
		t.Fatalf("control broken: @default must reach the schema, got %v", got)
	}
	// A payload omitting the defaulted field validates -- which is the whole
	// point: the schema does not require it and does not fill it. If a future
	// change makes the field required, or makes validation supply the value,
	// this is where it surfaces.
	if _, isRequired := requiredSet(t, declaredMetadataFixture)["tier"]; isRequired {
		t.Error("a @default field became required, which is a behaviour change the reference " +
			"does not describe")
	}

	// The parsing half is real but narrower than the reference used to claim:
	// numbers and booleans are coerced, and there is NO datetime branch -- so a
	// datetime field takes a bool default without complaint. Pinned so the
	// corrected sentence in section 8 cannot drift back.
	for _, tc := range []struct {
		name string
		want any
	}{
		{"true", true},
		{"7", int64(7)},
		{"1.5", 1.5},
		{"2026-01-02T03:04:05Z", "2026-01-02T03:04:05Z"}, // NOT parsed as a time
	} {
		if got := parseDefaultValue(tc.name); got != tc.want {
			t.Errorf("parseDefaultValue(%q) = %#v, want %#v -- section 8 describes exactly this "+
				"set, including that RFC3339 is left as a string", tc.name, got, tc.want)
		}
	}
}

func requiredSet(t *testing.T, src string) map[string]struct{} {
	t.Helper()
	c := buildFixtureConcept(t, src)
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]struct{}{}
	for _, r := range doc.Required {
		out[r] = struct{}{}
	}
	return out
}

// repoRoot walks up from this package to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the module root, so this scan measures nothing")
	return ""
}

// grepNonTestGo returns "path:line" for every occurrence of needle in a
// non-test .go file under root, skipping vendor and the SDK (generated).
func grepNonTestGo(t *testing.T, root, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable path is not this test's business
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, needle) {
				hits = append(hits, rel+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return hits
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
