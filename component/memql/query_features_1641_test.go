package memql

import (
	"log/slog"
	"os"
	"testing"
)

// TestKnowledgeBridgeFullShapeRegistered is the #1641(c) regression guard.
//
// queryKnowledgeBridgeById (dsl/knowledge/queries.memql) projects through the
// `knowledgeBridgeFull` shape. Before #1641 that shape was never defined, so
// the query failed at execution with `shape template "knowledgeBridgeFull"
// not found` -- a lazy resolution error the engine-bootstrap smoke test does
// NOT catch (shape templates resolve at query time, not at Init). This test
// loads the unified shape tree and asserts the shape is registered, turning
// the runtime miss into a unit-test failure.
func TestKnowledgeBridgeFullShapeRegistered(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	shapeReg := newShapeRegistry()
	if _, err := LoadUnifiedShapes(logger, shapeReg); err != nil {
		t.Fatalf("LoadUnifiedShapes: %v", err)
	}

	shape, ok := shapeReg.Get("knowledgeBridgeFull")
	if !ok {
		t.Fatalf("shape registry missing %q -- queryKnowledgeBridgeById will fail at "+
			"runtime with `shape template \"knowledgeBridgeFull\" not found` (#1641c)", "knowledgeBridgeFull")
	}

	// The bridge lookup is only useful if the projection carries the
	// combination-key fields the training pipeline reads back. The template is
	// keyed by each path's terminal segment (payload.bridgeId -> "bridgeId").
	// Guard the load-bearing ones so a future field trim doesn't silently gut
	// the shape.
	for _, w := range []string{"bridgeId", "roleSlug", "domainIds", "combinationKey"} {
		if _, ok := shape.Template[w]; !ok {
			t.Errorf("knowledgeBridgeFull missing expected field %q", w)
		}
	}
}
