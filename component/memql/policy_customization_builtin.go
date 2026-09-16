package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (e *MemQLEngine) policyCatalogBuiltin(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" {
		return nil, fmt.Errorf("policy catalog requires sign-in")
	}
	catalog, err := e.policies.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]memorynodes.MemoryNode, 0, len(catalog))
	for _, p := range catalog {
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		rows = append(rows, memorynodes.MemoryNode{ID: "policy:" + p.Name, Concept: "v1:router:policyCatalog", Payload: raw})
	}
	return rows, nil
}
func policyExpectedRevision(args map[string]any) (int64, error) {
	var n int64
	switch v := args["expectedRevision"].(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 9007199254740991 || math.Trunc(v) != v {
			return 0, fmt.Errorf("expectedRevision must be a nonnegative integer")
		}
		n = int64(v)
	default:
		return 0, fmt.Errorf("expectedRevision is required; read the policy catalog first")
	}
	if n < 0 {
		return 0, fmt.Errorf("expectedRevision must be nonnegative")
	}
	return n, nil
}
func (e *MemQLEngine) policySaveBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	revision, err := policyExpectedRevision(args)
	if err != nil {
		return nil, err
	}
	fallbacks := []string{}
	if raw, ok := args["fallbacks"]; ok && raw != nil {
		switch value := raw.(type) {
		case []string:
			fallbacks = value
		case []any:
			for _, v := range value {
				entry, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("every fallback must be a source string")
				}
				fallbacks = append(fallbacks, entry)
			}
		default:
			return nil, fmt.Errorf("fallbacks must be an array")
		}
	}
	p := PolicyConfig{Name: stringArg(args, "name"), Description: stringArg(args, "description"), Primary: stringArg(args, "primary"), Fallbacks: fallbacks}
	if err := e.policies.Save(ctx, revision, p); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:router:policyCatalog", p.Name, map[string]any{"name": p.Name, "revision": revision + 1, "status": "saved"})
}
func (e *MemQLEngine) policyResetBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	revision, err := policyExpectedRevision(args)
	if err != nil {
		return nil, err
	}
	name := stringArg(args, "name")
	if err := e.policies.Reset(ctx, revision, name); err != nil {
		return nil, err
	}
	return singleVirtualRow("v1:router:policyCatalog", name, map[string]any{"name": name, "revision": revision + 1, "status": "reset"})
}
