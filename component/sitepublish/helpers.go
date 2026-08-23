package sitepublish

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// Row-shaped accessors, and the synthetic result concept.
//
// These four are copies of integrations/library's own unexported helpers,
// which is where this capability lived until the module boundary forced the
// move (memql#4345). The move was not stylistic: component/edge lives in the
// ROOT module, integrations is its own module, and the root module already
// requires integrations -- so importing component/edge from integrations/library
// made the module graph a cycle. `GOWORK=off go build` is what says so; the
// workspace resolves it and hides the problem, which is exactly what CI's
// module-boundaries lane exists to catch.
//
// Copied rather than exported from integrations/library, because exporting them
// would give this package a dependency on that one for four lines of map access
// -- re-coupling the two packages the move just separated, in the direction that
// would put the cycle back the moment either grew.

// resultConcept is the synthetic MemoryNode concept the capability returns.
// Never persisted: the engine round-trips it back to the caller and discards
// it, which is why the value is unchanged from the capability's previous home
// rather than renamed -- nothing reads it, and a rename would be a wire change
// made for tidiness.
const resultConcept = "integration:library:result"

func asString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// extractRows normalises whatever the engine handed back into plain rows.
// Copied from integrations/library for the reason in this file's header.
func extractRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	if res, ok := raw.(*memql.ExecuteResult); ok && res != nil {
		raw = res.OutputPayload()
	}
	if raw == nil {
		return nil
	}
	if bundle, ok := raw.(*memqlv1.GraphBundle); ok && bundle != nil {
		out := make([]map[string]any, 0, len(bundle.GetNodes()))
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{
				"id":      n.GetId(),
				"concept": n.GetConcept(),
			}
			if payload := n.GetPayload(); payload != nil {
				for k, v := range payload.AsMap() {
					row[k] = v
				}
			}
			out = append(out, row)
		}
		return out
	}
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		if bundle, ok := v["bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				out := make([]map[string]any, 0, len(nodes))
				for _, n := range nodes {
					if m, ok := n.(map[string]any); ok {
						out = append(out, m)
					}
				}
				return out
			}
		}
		return []map[string]any{v}
	}
	return nil
}
