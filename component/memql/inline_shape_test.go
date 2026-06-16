package memql

// Phase 5 (#1535) parser relaxation tests: the inline-definition (`name := expr`)
// and trailing `@timestamp`/`@latest` shapes are pre-rejected by default, and the
// pre-rejection is lifted under allowInline (the MCP Tier-3 path). Pure -- no DB.

import (
	"errors"
	"testing"
)

func TestUnsupportedQueryShape_InlineGate(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"inline spec def", `mySpec := payload.active==true`},
		{"trailing @timestamp", `concept==v1:cognition:space@2024-01-01T00:00:00Z`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Default (sealed) path pre-rejects with the typed shape error.
			if err := unsupportedQueryShape(c.query, false); !errors.Is(err, ErrUnsupportedQueryShape) {
				t.Errorf("allowInline=false should pre-reject %q, got %v", c.query, err)
			}
			// Inline path lifts the pre-rejection (the langparser-grammar gap for
			// these shapes is a separate, tracked follow-up).
			if err := unsupportedQueryShape(c.query, true); err != nil {
				t.Errorf("allowInline=true should lift the pre-rejection for %q, got %v", c.query, err)
			}
		})
	}
}

// The lift is SHAPE-SCOPED: other unsupported shapes (e.g. select(...)) stay
// rejected even under allowInline -- the inline tier doesn't blanket-disable the
// detector.
func TestUnsupportedQueryShape_InlineDoesNotLiftOthers(t *testing.T) {
	if err := unsupportedQueryShape(`select(payload.name)`, true); !errors.Is(err, ErrUnsupportedQueryShape) {
		t.Errorf("allowInline must NOT lift select(...) rejection, got %v", err)
	}
}

// A plain inline query (the common case) is accepted on BOTH paths -- the
// restriction only ever bit the two named exotic shapes.
func TestUnsupportedQueryShape_PlainQueryAlwaysOK(t *testing.T) {
	for _, allow := range []bool{false, true} {
		if err := unsupportedQueryShape(`concept==v1:cognition:space && payload.status=="active"`, allow); err != nil {
			t.Errorf("plain inline query rejected (allowInline=%v): %v", allow, err)
		}
	}
}
