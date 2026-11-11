package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// export.go -- ONE STORE'S WHOLE WHOLESALE RECORD (wholesaleExportStore).
//
// One JSON file holding every application the caller reads -- each with the
// client's own fields on it and the state its log gives it -- their decisions
// and entitlements, and the store's settings, so a store can keep its record or
// take it elsewhere.
//
// THE SAME RULE AS THE LIST. The applications are read under the caller, so
// what a caller cannot read is not in their export; an application's decisions
// and entitlement are then read as the deployment, so the record holds its
// whole log (X27). The settings are read as the deployment once siteForStore
// has checked the site is the store's.
//
// A TOKEN HASH NEVER LEAVES: it is a credential's digest, not the store's data,
// and every row is stripped of one, whatever concept a later task adds.

// exportFormat names the file's shape, so a later reader knows what it holds.
const exportFormat = "memql.wholesale.export/v1"

// tokenHashField is the field a credential's digest is kept in.
const tokenHashField = "tokenHash"

func (p *Provider) exportStore(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, _, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	// THE WHOLE STORE, page after page: an export that stopped at a thousand
	// would be a record that silently lost its oldest applications.
	var apps []map[string]any
	cursor := ""
	for {
		rows, next, err := p.rowsPage(ctx, "applicationsForStore", map[string]string{"storeId": storeID}, cursor)
		if err != nil {
			return nil, err
		}
		apps = append(apps, rows...)
		if next == "" {
			break
		}
		cursor = next
	}
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	applications := make([]map[string]any, 0, len(apps))
	decisions := []map[string]any{}
	entitlements := []map[string]any{}
	for _, app := range apps {
		id := trimmed(app["id"])
		if id == "" {
			continue
		}
		log, err := p.rowsForApplication(sys, "decisionsForApplication", id)
		if err != nil {
			return nil, err
		}
		details, err := p.details(ctx, app)
		if err != nil {
			return nil, err
		}
		out := withoutTokenHash(app)
		out["details"] = details
		out["state"] = FoldState(log)
		out["stage"] = StageOf(FoldState(log), false, false)
		applications = append(applications, out)
		for _, d := range log {
			decisions = append(decisions, withoutTokenHash(d))
		}
		ent, err := p.entitlementRow(sys, id)
		if err != nil {
			return nil, err
		}
		if ent != nil {
			entitlements = append(entitlements, withoutTokenHash(ent))
		}
	}
	settings, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return nil, err
	}
	var settingsOut any
	if settings != nil {
		settingsOut = withoutTokenHash(settings)
	}
	now := time.Now().UTC()
	if p.now != nil {
		now = p.now().UTC()
	}
	doc, err := json.MarshalIndent(map[string]any{
		"format":       exportFormat,
		"exportedAt":   now.Format(time.RFC3339Nano),
		"storeId":      storeID,
		"applications": applications,
		"decisions":    decisions,
		"entitlements": entitlements,
		"settings":     settingsOut,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("wholesale: marshal export: %w", err)
	}
	return confirm("wholesale:export", map[string]any{
		"fileName": "wholesale-" + slugOf(storeID) + "-" + now.Format("20060102") + ".json",
		"json":     string(doc),
	})
}

// withoutTokenHash is a copy of row with every token hash removed, at any depth.
func withoutTokenHash(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		if k == tokenHashField {
			continue
		}
		out[k] = stripTokenHash(v)
	}
	return out
}

func stripTokenHash(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return withoutTokenHash(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = stripTokenHash(item)
		}
		return out
	}
	return v
}
