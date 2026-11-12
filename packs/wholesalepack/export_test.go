package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
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

func (h hookedWriter) Insert(ctx context.Context, mutation string, args map[string]any) (string, error) {
	if h.before != nil {
		h.before(ctx, mutation, args)
	}
	return h.inner.Insert(ctx, mutation, args)
}

// CurrentDocumentsForTest answers currentDocuments over eng.
func CurrentDocumentsForTest(ctx context.Context, eng memql.IntegrationEngineAccess, storeID string) (map[string]map[string]any, error) {
	p := &Provider{engine: eng, reader: &engineRowReader{engine: eng}, writer: &engineWriter{engine: eng}}
	return p.currentDocuments(ctx, storeID)
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

// SetSettingsForTest runs wholesaleSetSettings over eng with a hook run before
// its write.
func SetSettingsForTest(ctx context.Context, eng memql.IntegrationEngineAccess, args map[string]any,
	before func(ctx context.Context, mutation string, args map[string]any)) ([]byte, error) {
	p := &Provider{
		engine: eng,
		reader: &engineRowReader{engine: eng},
		writer: hookedWriter{inner: &engineWriter{engine: eng}, before: before},
	}
	nodes, err := p.setWholesaleSettings(ctx, args, 0)
	if err != nil || len(nodes) == 0 {
		return nil, err
	}
	return nodes[0].Payload, nil
}

// MintLinkForTest mints a link of purpose for the application at applicationID,
// read as the deployment, as Task 14's resolver will, and answers its token.
func MintLinkForTest(ctx context.Context, eng memql.IntegrationEngineAccess, applicationID, purpose string) (string, error) {
	p := &Provider{engine: eng, reader: &engineRowReader{engine: eng}, writer: &engineWriter{engine: eng}}
	rows, err := p.rowsFor(auth.ContextWithSystemActor(ctx, provisionActor), "applicationById",
		map[string]string{"applicationId": applicationID})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("no application %q", applicationID)
	}
	token, _, err := p.mintLink(ctx, rows[0], purpose, p.clock())
	return token, err
}

// hookedReader runs atCurrent once, the first time the store's current
// documents are read: in the accept path, after the acceptance read.
type hookedReader struct {
	rowReader
	once      *sync.Once
	atCurrent func()
}

func (h hookedReader) RowsPageWithList(ctx context.Context, query string, args map[string]string, listName string, list []string,
	cursor string) ([]map[string]any, string, error) {
	if query == documentBriefsQuery && h.atCurrent != nil {
		h.once.Do(h.atCurrent)
	}
	return h.rowReader.RowsPageWithList(ctx, query, args, listName, list, cursor)
}

// AcceptTermsForTest runs wholesaleAcceptTerms over eng, running atCurrent once
// past the acceptance read, before the current versions are read and the
// acceptance written.
func AcceptTermsForTest(ctx context.Context, eng memql.IntegrationEngineAccess, args map[string]any, atCurrent func()) error {
	p := &Provider{engine: eng, writer: &engineWriter{engine: eng},
		reader: hookedReader{rowReader: &engineRowReader{engine: eng}, once: &sync.Once{}, atCurrent: atCurrent}}
	_, err := p.acceptTerms(ctx, args, 0)
	return err
}
