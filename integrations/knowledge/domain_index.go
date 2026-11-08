package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type domainCount struct {
	DomainID    string `json:"domainId"`
	Total       int    `json:"total"`
	Validated   int    `json:"validated"`
	Rejected    int    `json:"rejected"`
	Unvalidated int    `json:"unvalidated"`
}

// Cursor reduction is one authorized read operation. It holds only domain
// counters between bounded pages; source-row admission remains the engine's,
// before any row contributes to the synthetic result. No caller is elevated.
func (i *Integration) trainingDomainRollups(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := auth.AccessFromContext(ctx)
	if access == nil || access.UserId == "" {
		return nil, fmt.Errorf("training domain counts require an authenticated caller")
	}
	if access.Unranked || auth.RoleRank(access.Role) < auth.RoleRank(auth.RoleWriter) {
		return nil, fmt.Errorf("training domain counts require writer access")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("knowledge engine is unavailable")
	}
	asOf, _ := json.Marshal(time.Now().UTC().Format(time.RFC3339Nano))
	call := fmt.Sprintf("query allDocumentChunkDomains(asOf: %s)", asOf)
	counts := map[string]*domainCount{}
	err := memql.WalkQueryPages(ctx, i.engine.Execute, call, 10000, func(result *memql.ExecuteResult) error {
		for _, row := range memql.MaterializeRows(result.OutputPayload()) {
			domain, _ := row["domainId"].(string)
			if domain == "" {
				continue
			}
			count := counts[domain]
			if count == nil {
				count = &domainCount{DomainID: domain}
				counts[domain] = count
			}
			count.Total++
			switch row["validationStatus"] {
			case "validated":
				count.Validated++
			case "rejected":
				count.Rejected++
			default:
				count.Unvalidated++
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read training domain counts: %w", err)
	}
	rollups := make([]domainCount, 0, len(counts))
	for _, count := range counts {
		rollups = append(rollups, *count)
	}
	sort.Slice(rollups, func(a, b int) bool { return rollups[a].DomainID < rollups[b].DomainID })
	payload, err := json.Marshal(map[string]any{"rollups": rollups})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "knowledge-domain-counts", Concept: "integration:knowledge:domainCounts", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
