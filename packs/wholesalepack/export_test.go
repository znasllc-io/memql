package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// export_test.go -- seams the external database suite drives the backfill
// through: the same reader and writer the provider uses over a real engine,
// with the walk's window chosen and a hook run before each write.

// hookedWriter runs before ahead of each write, then writes.
type hookedWriter struct {
	inner  rowWriter
	before func(ctx context.Context, mutation string, args map[string]any)
}

func (h hookedWriter) Write(ctx context.Context, mutation string, args map[string]any) error {
	if h.before != nil {
		h.before(ctx, mutation, args)
	}
	return h.inner.Write(ctx, mutation, args)
}

// BackfillForTest runs wholesaleBackfillAccounts over eng with the given walk
// window and write hook, and answers its report.
func BackfillForTest(ctx context.Context, eng memql.IntegrationEngineAccess, args map[string]any, window int,
	before func(ctx context.Context, mutation string, args map[string]any)) (map[string]any, error) {
	p := &Provider{
		engine:         eng,
		reader:         &engineRowReader{engine: eng},
		writer:         hookedWriter{inner: &engineWriter{engine: eng}, before: before},
		caller:         &engineCaller{engine: eng},
		backfillWindow: window,
	}
	nodes, err := p.backfillAccounts(ctx, args, 0)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 {
		return nil, fmt.Errorf("the backfill answered %d nodes, want 1", len(nodes))
	}
	out := map[string]any{}
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateSettingsForTest runs wholesaleUpdateSettings over eng with a hook run
// before its write, so a test can land a concurrent save between its read and
// its write.
func UpdateSettingsForTest(ctx context.Context, eng memql.IntegrationEngineAccess, args map[string]any,
	before func(ctx context.Context, mutation string, args map[string]any)) ([]byte, error) {
	p := &Provider{
		engine: eng,
		reader: &engineRowReader{engine: eng},
		writer: hookedWriter{inner: &engineWriter{engine: eng}, before: before},
	}
	nodes, err := p.updateSettings(ctx, args, 0)
	if err != nil || len(nodes) == 0 {
		return nil, err
	}
	return nodes[0].Payload, nil
}
