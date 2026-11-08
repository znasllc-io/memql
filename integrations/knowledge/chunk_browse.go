package knowledge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type chunkBrowseCursor struct {
	AsOf    string `json:"asOf"`
	Search  string `json:"search"`
	Pending bool   `json:"pending"`
	Page    string `json:"page"`
	Offset  int    `json:"offset"`
}

// The shared index is already authorized to enumerate every domain. It supplies
// only metadata here; full text is read for at most ten IDs through the existing
// required-domain read. Never add an all-domains escape to that full-text query.
func (i *Integration) trainingChunksBrowse(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := auth.AccessFromContext(ctx)
	if access == nil || access.UserId == "" || access.Unranked || auth.RoleRank(access.Role) < auth.RoleRank(auth.RoleWriter) {
		return nil, fmt.Errorf("training review requires writer access")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("knowledge engine is unavailable")
	}
	search, _ := args["search"].(string)
	search = strings.TrimSpace(search)
	if len(search) > 512 {
		return nil, fmt.Errorf("training search is too long")
	}
	pending, _ := args["pending"].(bool)
	cursor := chunkBrowseCursor{AsOf: time.Now().UTC().Format(time.RFC3339Nano), Search: search, Pending: pending}
	if token, _ := args["cursor"].(string); token != "" {
		if len(token) > 8192 {
			return nil, fmt.Errorf("invalid training cursor")
		}
		body, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || json.Unmarshal(body, &cursor) != nil || cursor.Offset < 0 || cursor.Offset >= 500 || cursor.Search != search || cursor.Pending != pending {
			return nil, fmt.Errorf("training cursor does not match this search")
		}
		if _, err = time.Parse(time.RFC3339Nano, cursor.AsOf); err != nil {
			return nil, fmt.Errorf("invalid training cursor time")
		}
	}
	quote := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	result, err := i.engine.Execute(memql.ContextWithCursor(memql.ContextWithFreshRead(ctx), cursor.Page), fmt.Sprintf("query allDocumentChunkDomains(asOf:%s,pending:%t,search:%s)", quote(cursor.AsOf), pending, quote(search)))
	if err != nil {
		return nil, err
	}
	if meta := result.GetMeta(); meta != nil && meta.HasMore && (meta.Cursor == "" || meta.Cursor == cursor.Page) {
		return nil, fmt.Errorf("training source cursor did not advance")
	}
	metadata := memql.MaterializeRows(result.OutputPayload())
	if cursor.Offset > len(metadata) {
		return nil, fmt.Errorf("training source page changed; refresh the list")
	}
	end := min(cursor.Offset+10, len(metadata))
	selected := metadata[cursor.Offset:end]
	byDomain := map[string][]string{}
	for _, row := range selected {
		domain, _ := row["domainId"].(string)
		id, _ := row["id"].(string)
		if domain == "" || id == "" {
			return nil, fmt.Errorf("training source returned incomplete metadata")
		}
		byDomain[domain] = append(byDomain[domain], id)
	}
	full := map[string]map[string]any{}
	for domain, ids := range byDomain {
		page, err := i.engine.Execute(memql.ContextWithCursor(memql.ContextWithFreshRead(ctx), ""), fmt.Sprintf("query documentChunksForDomain(domainId:%s,ids:%s,asOf:%s)", quote(domain), quote(ids), quote(cursor.AsOf)))
		if err != nil {
			return nil, err
		}
		for _, row := range memql.MaterializeRows(page.OutputPayload()) {
			id, _ := row["id"].(string)
			full[id] = row
		}
	}
	rows := make([]map[string]any, 0, len(selected))
	for _, row := range selected {
		id, _ := row["id"].(string)
		if item := full[id]; item != nil {
			rows = append(rows, item)
		}
	}
	next := ""
	if end < len(metadata) {
		cursor.Offset = end
	} else {
		cursor.Offset = 0
		cursor.Page = ""
		if result.GetMeta() != nil {
			cursor.Page = result.GetMeta().Cursor
		}
	}
	if end < len(metadata) || cursor.Page != "" {
		body, _ := json.Marshal(cursor)
		next = base64.RawURLEncoding.EncodeToString(body)
	}
	body, err := json.Marshal(map[string]any{"rows": rows, "nextCursor": next})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "training-chunks-page", Concept: "integration:knowledge:chunkPage", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: body}}, nil
}
