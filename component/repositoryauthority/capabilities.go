package repositoryauthority

import (
	"context"
	"encoding/json"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func init() {
	memql.RegisterPlugin("repositoryauthority", func(p memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(Deps{SystemVariable: p.ResolveSystemVariable}), nil
	})
}
func decoded[T any](v any) (T, error) {
	var out T
	b, e := json.Marshal(v)
	if e == nil {
		e = json.Unmarshal(b, &out)
	}
	return out, e
}
func result(v any, e error) ([]memorynodes.MemoryNode, error) {
	if e != nil {
		return nil, e
	}
	m, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return []memorynodes.MemoryNode{{Concept: "v1:repositoryauthority:result", Type: memorynodes.NodeTypeObject, Payload: m}}, nil
}
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "context", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			v, e := i.ReadContext(ctx, str(a["requestId"]))
			return result(v, e)
		}},
		{Name: "access", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			v, e := i.Access(ctx, str(a["sourceId"]))
			return result(v, e)
		}},
		{Name: "request", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			r, e := decoded[Request](a)
			if e != nil {
				return nil, e
			}
			v, e := i.Request(ctx, r)
			return result(v, e)
		}},
		{Name: "receive", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			r, e := decoded[Transfer](a["transfer"])
			if e != nil {
				return nil, e
			}
			v, e := i.Receive(ctx, r)
			return result(v, e)
		}},
		{Name: "status", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			v, e := i.Status(ctx, str(a["requestId"]))
			return result(v, e)
		}},
		{Name: "requests", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			v, e := i.List(ctx, str(a["sourceId"]))
			return result(map[string]any{"requests": v}, e)
		}},
		{Name: "revoke", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			v, e := i.Revoke(ctx, str(a["requestId"]), str(a["reason"]))
			return result(v, e)
		}},
		{Name: "propose", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			p, e := decoded[Proposal](a)
			if e != nil {
				return nil, e
			}
			v, e := i.Propose(ctx, p)
			return result(v, e)
		}},
		// A direct probe never grants a writer. Installed templates may inspect only
		// whether their exact run can proceed; the writer repeats native authorization.
		{Name: "authorize", Handler: func(ctx context.Context, a map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			paths, e := decoded[[]string](a["paths"])
			if e != nil {
				return nil, e
			}
			_, e = i.AuthorizeEffect(ctx, str(a["requestId"]), str(a["operation"]), paths)
			if e != nil {
				return nil, e
			}
			return result(map[string]any{"authorized": true}, nil)
		}},
	}
}
