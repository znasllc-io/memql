package memql

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// TestMalformedConstructWarnsAtLoad is the memql#2356 fail-loud guard
// for the load side: a garbage-body spec / shape (the two kinds the
// epic #2351 audit proved failed SILENTLY) now emits a WARN through
// the shared baseloader pipeline -- captured by the same captureHandler
// the load-clean gate uses -- instead of vanishing at Debug. It drives
// the exact production loader closures (LoadUnifiedSpecs /
// LoadUnifiedShapes) against an in-memory garbage file so the assertion
// exercises the real parse + Warn path, not a mock.
//
// Pre-#2356 these parse failures logged at Debug and were invisible to
// (a) production logs, (b) TestEmbeddedDSLLoadsCleanly (WARN+ only),
// and (c) the CI lint gate (strip removed the body before parsing).
func TestMalformedConstructWarnsAtLoad(t *testing.T) {
	// The exact garbage-spec body called out in the epic #2351 audit.
	const garbageSpec = "@enabled\n@description(\"bad\")\nspec activeRowTrait specBad {\n" +
		"  return status ==== \"x\" &&&& true\n}\n"
	// Balanced braces (so slice extraction still finds the block) but a
	// body the shape parser rejects: numeric tokens where field paths
	// are required.
	const garbageShape = "@row\n@description(\"bad\")\nshape shapeBad {\n" +
		"  row.id\n  123 456 789\n}\n"

	t.Run("spec", func(t *testing.T) {
		capture := newCaptureHandler()
		logger := slog.New(capture)
		files := []baseloader.RawFile{{Path: "cognition/specs.memql", Content: garbageSpec}}
		parse := func(origin string, raw []byte) (*Spec, error) {
			decl, err := languageParser.ParseSpecDecl(string(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", origin, err)
			}
			return specDeclToSpec(decl, origin)
		}
		reg := newSpecRegistry()
		n, err := baseloader.LoadOne[Spec](logger, "memql.unifiedSpecLoader", "spec",
			files, extractAdapter, parse, reg.add)
		if err != nil {
			t.Fatalf("LoadOne returned pipeline error: %v", err)
		}
		if n != 0 {
			t.Errorf("garbage spec should register 0 constructs, got %d", n)
		}
		assertWarnCaptured(t, capture, "parse failed", "specBad")
	})

	t.Run("shape", func(t *testing.T) {
		capture := newCaptureHandler()
		logger := slog.New(capture)
		files := []baseloader.RawFile{{Path: "cognition/shapes.memql", Content: garbageShape}}
		parse := func(origin string, raw []byte) (*ShapeDefinition, error) {
			decl, err := languageParser.ParseShapeDecl(string(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", origin, err)
			}
			shape, err := shapeDeclToShapeDefinition(decl, origin)
			if err != nil {
				return nil, err
			}
			shape.Origin = strings.TrimSuffix(origin, ":"+shape.Name)
			return shape, nil
		}
		reg := newShapeRegistry()
		n, err := baseloader.LoadOne[ShapeDefinition](logger, "memql.unifiedShapeLoader", "shape",
			files, extractAdapter, parse, reg.Upsert)
		if err != nil {
			t.Fatalf("LoadOne returned pipeline error: %v", err)
		}
		if n != 0 {
			t.Errorf("garbage shape should register 0 constructs, got %d", n)
		}
		assertWarnCaptured(t, capture, "parse failed", "shapeBad")
	})
}

// assertWarnCaptured fails the test unless the capture handler recorded
// a WARN whose message contains msgSubstr AND whose attributes name the
// offending construct (nameSubstr appears in any string attr value). It
// also asserts the record is exactly at WARN, proving the Debug->Warn
// raise took effect. Finally it confirms the same record is what
// captureHandler.skipLike surfaces, so the load-clean gate would catch it.
func assertWarnCaptured(t *testing.T, capture *captureHandler, msgSubstr, nameSubstr string) {
	t.Helper()
	capture.mu.Lock()
	records := append([]capturedRecord(nil), capture.records...)
	capture.mu.Unlock()

	for _, rec := range records {
		if rec.Level != slog.LevelWarn {
			continue
		}
		if !strings.Contains(rec.Message, msgSubstr) {
			continue
		}
		named := false
		for _, v := range rec.Attrs {
			if s, ok := v.(string); ok && strings.Contains(s, nameSubstr) {
				named = true
				break
			}
		}
		if named {
			// The load-clean gate keys off skipLike; prove it catches this.
			if len(capture.skipLike()) == 0 {
				t.Errorf("record matched but skipLike() returned nothing -- the load-clean gate would miss it: %s", rec.format())
			}
			return
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("no WARN record with message %q naming %q was captured; got:\n", msgSubstr, nameSubstr))
	for _, rec := range records {
		sb.WriteString("  " + rec.format() + "\n")
	}
	t.Error(sb.String())
}
