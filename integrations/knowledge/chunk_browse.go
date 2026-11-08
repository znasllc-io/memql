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
	Version int    `json:"v"`
	AsOf    string `json:"asOf"`
	Search  string `json:"search"`
	Pending bool   `json:"pending"`
	Page    string `json:"page"`
}

// The metadata index is authorized to enumerate every domain, but this reader
// uses its own ten-row keyset window. A positional offset inside a larger page
// skips unread rows when earlier chunks are approved or removed. Full text is
// read for at most ten IDs through the existing required-domain read; never add
// an all-domains escape to that full-text query.
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
	cursor := chunkBrowseCursor{Version: 1, AsOf: time.Now().UTC().Format(time.RFC3339Nano), Search: search, Pending: pending}
	if token, _ := args["cursor"].(string); token != "" {
		if len(token) > 8192 {
			return nil, fmt.Errorf("invalid training cursor")
		}
		body, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || json.Unmarshal(body, &cursor) != nil || cursor.Version != 1 || cursor.Page == "" || cursor.Search != search || cursor.Pending != pending {
			return nil, fmt.Errorf("training cursor does not match this search")
		}
		if asOf, parseErr := time.Parse(time.RFC3339Nano, cursor.AsOf); parseErr != nil || asOf.After(time.Now().Add(time.Minute)) {
			return nil, fmt.Errorf("invalid training cursor time")
		}
	}
	quote := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	// Narrow the established metadata read through the existing paginate
	// operator. This preserves its authority boundary and leaves its default
	// 500-row count-reducer window unchanged; no new unmeasured read surface.
	result, err := i.engine.Execute(memql.ContextWithCursor(memql.ContextWithFreshRead(ctx), cursor.Page), fmt.Sprintf("paginate(query allDocumentChunkDomains(asOf:%s,pending:%t,search:%s),10)", quote(cursor.AsOf), pending, quote(search)))
	if err != nil {
		return nil, err
	}
	if meta := result.GetMeta(); meta != nil && meta.HasMore && (meta.Cursor == "" || meta.Cursor == cursor.Page) {
		return nil, fmt.Errorf("training source cursor did not advance")
	}
	selected := memql.MaterializeRows(result.OutputPayload())
	if len(selected) > 10 {
		return nil, fmt.Errorf("training source exceeded its page limit")
	}
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
	cursor.Page = ""
	if result.GetMeta() != nil {
		cursor.Page = result.GetMeta().Cursor
	}
	if cursor.Page != "" {
		body, _ := json.Marshal(cursor)
		next = base64.RawURLEncoding.EncodeToString(body)
	}
	body, err := json.Marshal(map[string]any{"rows": rows, "nextCursor": next})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "training-chunks-page", Concept: "integration:knowledge:chunkPage", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: body}}, nil
}
