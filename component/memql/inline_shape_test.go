package memql

// Phase 5 (#1535) parser relaxation tests: the inline-definition (`name := expr`)
// and trailing `@timestamp`/`@latest` shapes are pre-rejected by default, and the
// pre-rejection is lifted under allowInline (the MCP Tier-3 path). Pure -- no DB.
//
// #1558 grammar tests: under allowInline the two shapes now PARSE (rather than
// surface `unexpected token after expression`) and produce the right plan -- the
// trailing @asOf modifier populates Timestamp/UseLatest, and the inline-spec
// definition materialises an InlineSpecs entry whose reference becomes the root.

import (
	"errors"
	"testing"
	"time"
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

// #1558: the trailing @timestamp asOf modifier now PARSES under allowInline and
// peels off into the langpath result's Timestamp; UseLatest stays false. Under
// allowInline=false the shape is still pre-rejected (it never reaches the parser).
func TestInlineGrammar_TrailingTimestampParses(t *testing.T) {
	res, err := parseViaLangparser(`concept==v1:cognition:space@2024-01-01T00:00:00Z`, true)
	if err != nil {
		t.Fatalf("trailing @timestamp should parse under allowInline, got: %v", err)
	}
	if res.UseLatest {
		t.Errorf("a concrete timestamp must not set UseLatest")
	}
	want, _ := time.Parse(time.RFC3339Nano, "2024-01-01T00:00:00Z")
	if res.Timestamp == nil || !res.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", res.Timestamp, want)
	}
	if _, ok := res.Root.(*ComparisonExpression); !ok {
		t.Errorf("body should be the comparison with the suffix stripped, got %T", res.Root)
	}

	// allowInline=false still rejects (the pre-flight detector fires).
	if _, err := parseViaLangparser(`concept==v1:cognition:space@2024-01-01T00:00:00Z`, false); !errors.Is(err, ErrUnsupportedQueryShape) {
		t.Errorf("allowInline=false must reject trailing @timestamp, got %v", err)
	}
}

// #1558: trailing @latest parses and sets UseLatest with a nil Timestamp.
func TestInlineGrammar_TrailingLatestParses(t *testing.T) {
	res, err := parseViaLangparser(`concept==v1:cognition:space @latest`, true)
	if err != nil {
		t.Fatalf("trailing @latest should parse under allowInline, got: %v", err)
	}
	if !res.UseLatest || res.Timestamp != nil {
		t.Errorf("@latest must set UseLatest with nil Timestamp, got useLatest=%v ts=%v", res.UseLatest, res.Timestamp)
	}
}

// #1558: a missing/empty value after the trailing @ is a clean error, not a
// silent drop.
func TestInlineGrammar_TrailingAsOfRequiresValue(t *testing.T) {
	if _, err := parseViaLangparser(`concept==v1:cognition:space@`, true); err == nil {
		t.Errorf("a bare trailing @ should error (requires a value)")
	}
	if _, err := parseViaLangparser(`concept==v1:cognition:space@not-a-timestamp`, true); err == nil {
		t.Errorf("an invalid @asOf value should error")
	}
}

// #1558: the inline-spec definition `name := expr` now PARSES under allowInline.
// It materialises an InlineSpecs entry and rewrites the root to a reference to
// that spec, so the existing resolvePlanSpecs inline path expands it. Under
// allowInline=false the shape is still pre-rejected.
func TestInlineGrammar_InlineSpecDefinitionParses(t *testing.T) {
	res, err := parseViaLangparser(`mySpec := payload.active==true`, true)
	if err != nil {
		t.Fatalf("inline spec definition should parse under allowInline, got: %v", err)
	}
	if len(res.InlineSpecs) != 1 {
		t.Fatalf("expected exactly one inline spec, got %d", len(res.InlineSpecs))
	}
	spec := res.InlineSpecs["mySpec"]
	if spec == nil {
		t.Fatalf("inline spec %q not materialised: %#v", "mySpec", res.InlineSpecs)
	}
	if spec.Kind != SpecKindRow {
		t.Errorf("payload-only predicate should classify as a row-spec, got %q", spec.Kind)
	}
	ref, ok := res.Root.(*SpecReferenceExpression)
	if !ok || ref.Name != "mySpec" {
		t.Errorf("root should reference the inline spec, got %T", res.Root)
	}

	// allowInline=false still rejects.
	if _, err := parseViaLangparser(`mySpec := payload.active==true`, false); !errors.Is(err, ErrUnsupportedQueryShape) {
		t.Errorf("allowInline=false must reject the inline spec definition, got %v", err)
	}
}

// #1558: a plain inline query carries NO modifiers -- the two shapes must not
// false-positive on an ordinary filter.
func TestInlineGrammar_PlainQueryHasNoModifiers(t *testing.T) {
	res, err := parseViaLangparser(`concept==v1:cognition:space && payload.status=="active"`, true)
	if err != nil {
		t.Fatalf("plain query parse err: %v", err)
	}
	if res.Timestamp != nil || res.UseLatest || res.InlineSpecs != nil {
		t.Errorf("plain query must have no asOf / inline-spec modifiers, got %#v", res)
	}
}

// #1558: the full Parse path threads the parsed modifiers onto the QueryPlan
// under allowInline -- Timestamp/UseLatest land on the plan and the inline spec
// resolves into the plan's root + InlineSpecs map. This is what the gated
// ExecuteInline path then executes (no further gate change).
func TestInlineGrammar_ParseWiresPlanModifiers(t *testing.T) {
	engine := newParserTestEngine(t)

	// Trailing @timestamp -> plan.Timestamp.
	plan, err := engine.parseWithFunctions(`concept==v1:cognition:space@2024-01-01T00:00:00Z`, engine.functions, nil, true)
	if err != nil {
		t.Fatalf("parse trailing @timestamp under allowInline: %v", err)
	}
	if plan.Timestamp == nil {
		t.Errorf("plan.Timestamp should be set from the trailing @asOf modifier")
	}

	// Trailing @latest -> plan.UseLatest.
	planLatest, err := engine.parseWithFunctions(`concept==v1:cognition:space @latest`, engine.functions, nil, true)
	if err != nil {
		t.Fatalf("parse trailing @latest under allowInline: %v", err)
	}
	if !planLatest.UseLatest {
		t.Errorf("plan.UseLatest should be set from @latest")
	}

	// Inline spec definition -> plan.InlineSpecs + expanded root.
	planSpec, err := engine.parseWithFunctions(`activeSpec := payload.active==true`, engine.functions, nil, true)
	if err != nil {
		t.Fatalf("parse inline spec definition under allowInline: %v", err)
	}
	if len(planSpec.InlineSpecs) != 1 || planSpec.InlineSpecs["activeSpec"] == nil {
		t.Errorf("plan.InlineSpecs should carry the inline spec, got %#v", planSpec.InlineSpecs)
	}
	// resolvePlanSpecs expands the SpecReference into the underlying predicate,
	// so the root is no longer a bare reference.
	if _, stillRef := planSpec.Root.(*SpecReferenceExpression); stillRef {
		t.Errorf("inline spec reference should have been expanded in the plan root")
	}

	// Both shapes still error WITHOUT allowInline on the full Parse path.
	if _, err := engine.parseWithFunctions(`concept==v1:cognition:space@2024-01-01T00:00:00Z`, engine.functions, nil, false); err == nil {
		t.Errorf("trailing @timestamp must error without allowInline on Parse")
	}
	if _, err := engine.parseWithFunctions(`activeSpec := payload.active==true`, engine.functions, nil, false); err == nil {
		t.Errorf("inline spec definition must error without allowInline on Parse")
	}
}
