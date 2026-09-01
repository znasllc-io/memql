package packages

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// memqlRows normalises whatever the engine handed back into plain rows.
//
// A THIRD copy of this shape (integrations/library has one, component/
// sitepublish copied it) and deliberately not a shared helper, for the reason
// sitepublish's own header records: these packages sit at different module
// tiers and the function is fifteen lines of type-switch over a wire type that
// does not change. What DOES change, and is the reason to keep reading it: a
// query carrying a SHAPE returns rows and nils the GraphBundle, so a caller
// that only handled the bundle branch would read an empty result from a
// perfectly good query (memql#4794 hit this the same way the Files epic did).
// Every branch below is reachable from some query in this package.
func memqlRows(raw any) []map[string]any {
	if raw == nil {
		return nil
	}
	if res, ok := raw.(*memql.ExecuteResult); ok {
		if res == nil {
			return nil
		}
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
			row := map[string]any{"id": n.GetId(), "concept": n.GetConcept()}
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
		if rows, ok := v["rows"].([]any); ok {
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
		return []map[string]any{v}
	}
	return nil
}
