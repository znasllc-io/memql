package library

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Reduce one authorized current population. The engine applies the installed
// query's owner, staged-data and row gates before any counts are computed.
func (i *Integration) handleBrowseFacets(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := auth.AccessFromContext(ctx)
	if access == nil || access.UserId == "" {
		return nil, fmt.Errorf("library facets require an authenticated caller")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("library engine is unavailable")
	}
	filters := map[string]any{"asOf": time.Now().UTC().Format(time.RFC3339Nano)}
	// Label options are intentionally independent of the selected label. All
	// remaining list constraints use the source query's ordinary owner gate.
	for _, key := range []string{"archived", "folderId", "kind", "source", "accountId", "search", "ids", "folderIds", "sourceRefs"} {
		if value, ok := args[key]; ok && value != nil {
			filters[key] = value
		}
	}
	rows, err := i.directoryRows(ctx, "libraryArtifactFacetRows", filters)
	if err != nil {
		return nil, err
	}
	folders := map[string]int{}
	labels := map[string]bool{}
	for _, row := range rows {
		folder, _ := row["folderId"].(string)
		folders[folder]++
		for _, label := range stringSliceField(row, "labels") {
			labels[label] = true
		}
	}
	folderIDs := make([]string, 0, len(folders))
	for folder := range folders {
		folderIDs = append(folderIDs, folder)
	}
	sort.Strings(folderIDs)
	counts := make([]map[string]any, 0, len(folderIDs))
	for _, folder := range folderIDs {
		counts = append(counts, map[string]any{"folderId": folder, "count": folders[folder]})
	}
	names := make([]string, 0, len(labels))
	for label := range labels {
		names = append(names, label)
	}
	sort.Strings(names)
	payload, err := json.Marshal(map[string]any{"folders": counts, "labelGroups": [][]string{names}})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "library-browse-facets", Concept: resultConcept, Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
