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
	// THE WHOLE STORE, page after page, in every form its id is held in: an
	// export that stopped at a thousand would be a record that silently lost
	// its oldest applications.
	apps, _, err := p.storeApplications(ctx, storeID, false)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(apps))
	for _, app := range apps {
		ids = append(ids, trimmed(app["id"]))
	}
	// Each application's whole log and entitlement, as the deployment, in
	// batches read to their end.
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	logs, err := p.rowsForApplications(sys, "decisionsForApplicationIds", ids)
	if err != nil {
		return nil, err
	}
	ents, err := p.rowsForApplications(sys, "entitlementsForApplicationIds", ids)
	if err != nil {
		return nil, err
	}
	applications := make([]map[string]any, 0, len(apps))
	decisions := []map[string]any{}
	entitlements := []map[string]any{}
	for _, app := range apps {
		id := trimmed(app["id"])
		if id == "" {
			continue
		}
		log := logs[bareID(id)]
		details, err := p.details(ctx, app)
		if err != nil {
			return nil, err
		}
		out := withoutTokenHash(app)
		out["details"] = details
		out["state"] = FoldState(log)
		out["stage"] = StageOf(FoldState(log), false, false)
		applications = append(applications, out)
		// ONE FORM OF THE APPLICATION'S ID throughout the file: the row's own,
		// whichever form an older decision or entitlement was written under.
		for _, d := range log {
			row := withoutTokenHash(d)
			row["applicationId"] = id
			decisions = append(decisions, row)
		}
		if versions := ents[bareID(id)]; len(versions) > 0 {
			row := withoutTokenHash(versions[0])
			row["applicationId"] = id
			entitlements = append(entitlements, row)
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
