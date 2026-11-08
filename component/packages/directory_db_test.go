package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestDirectoryRealDBKeepsVirtualAppsAndAuthorScope(t *testing.T) {
	engine, db := dbEngine(t)
	suffix := fmt.Sprintf("directory-%d", time.Now().UnixNano())
	owner := "v1:identity:user:" + suffix
	stranger := owner + "-other"
	seedTiePrincipal(t, engine, owner, auth.RoleWriter)
	seedTiePrincipal(t, engine, stranger, auth.RoleWriter)
	ids := []string{owner, stranger}
	defer func() {
		for _, id := range ids {
			_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	}()
	base := time.Now().UTC().Add(-time.Hour)
	for n := 0; n < 25; n++ {
		id := "v1:platform:package:" + fmt.Sprintf("%s-%02d", suffix, n)
		ids = append(ids, id)
		name := fmt.Sprintf("Recent %02d", n)
		if n < 12 {
			name = fmt.Sprintf("Earlier NEEDLE%%_ %02d", n)
		}
		restampSeed(t, db, "v1:platform:package", id, base.Add(time.Duration(n)*time.Minute), map[string]any{"ownerUserId": owner, "accountId": "", "name": name, "sourceKind": "repo", "repoUrl": "https://github.com/acme/" + fmt.Sprint(n), "status": "active", "declares": []any{map[string]any{"name": "app", "displayName": name, "kind": "spa"}, map[string]any{"name": "disabled", "kind": "static"}}, "disabledDeployables": []any{"disabled"}, "dslDomains": []any{strings.Repeat("private-unneeded-domain", 1000)}})
	}
	foreign := "v1:platform:package:" + suffix + "-foreign"
	ids = append(ids, foreign)
	restampSeed(t, db, "v1:platform:package", foreign, base.Add(2*time.Hour), map[string]any{"ownerUserId": stranger, "accountId": "", "name": "Earlier NEEDLE%_ foreign", "sourceKind": "repo", "repoUrl": "https://github.com/foreign/repo", "status": "active", "declares": []any{map[string]any{"name": "foreign", "kind": "spa"}}})
	integration := NewIntegration(engine, discardLogger())
	ctx := tieActorCtx(owner, auth.RoleWriter)
	walk := func(resource, search string, sizes []int) {
		cursor := ""
		seen := map[string]bool{}
		for page, size := range sizes {
			nodes, err := integration.handleDirectoryPage(ctx, map[string]any{"resource": resource, "search": search, "cursor": cursor}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(nodes[0].Payload, &result); err != nil {
				t.Fatal(err)
			}
			rows := result["entries"].([]any)
			if len(rows) != size {
				t.Fatalf("%s page%d=%d want%d payload=%s", resource, page, len(rows), size, nodes[0].Payload)
			}
			if len(nodes[0].Payload) > 24*1024 {
				t.Fatalf("unbounded page payload %d", len(nodes[0].Payload))
			}
			for _, raw := range rows {
				entry := raw.(map[string]any)
				id := entry["id"].(string)
				if seen[id] || strings.Contains(id, "foreign") {
					t.Fatal("duplicated or unauthorized row")
				}
				seen[id] = true
				if resource == "sources" {
					group := entry["group"].(map[string]any)
					summary := group["summary"].(map[string]any)
					if summary["apps"] != float64(2) || summary["deployed"] != float64(0) {
						t.Fatalf("source summary lost undeployed apps: %#v", summary)
					}
					if _, ok := group["pkg"].(map[string]any)["declares"]; ok {
						t.Fatal("source page transferred the app universe")
					}
				}
			}
			cursor = result["nextCursor"].(string)
			if (cursor != "") != (page < len(sizes)-1) {
				t.Fatal("bad cursor")
			}
		}
	}
	walk("sources", "", []int{10, 10, 5})
	walk("sources", "needle%_", []int{10, 2})
	walk("deployables", "", []int{10, 10, 10, 10, 10})
	walk("deployables", "needle%_", []int{10, 10, 4})
	// A stored site and a pending analysis replace their virtual declarations,
	// never duplicate them. Hydration still returns only directory identities.
	packageID := "v1:platform:package:" + suffix + "-00"
	siteID := "v1:platform:site:" + suffix + "-site"
	runID := "v1:platform:packageDeployment:" + suffix + "-pending"
	ids = append(ids, siteID, runID)
	restampSeed(t, db, "v1:platform:site", siteID, base.Add(30*time.Minute), map[string]any{"ownerUserId": owner, "packageId": packageID, "packageDeployableName": "app", "hostname": "directory.example.test", "kind": "spa", "status": "live", "settings": map[string]any{"large": strings.Repeat("private-settings", 10000)}})
	restampSeed(t, db, "v1:platform:packageDeployment", runID, base.Add(31*time.Minute), map[string]any{"ownerUserId": owner, "packageId": packageID, "requestedBy": owner, "status": "awaiting_confirm", "scopedTo": []any{"disabled"}, "report": map[string]any{"deployables": []any{map[string]any{"name": "disabled", "kind": "static", "buildPlan": strings.Repeat("large-plan", 10000)}}}, "buildLogTail": strings.Repeat("private-build-log", 10000)})
	nodes, err := integration.handleDirectoryPage(ctx, map[string]any{"resource": "deployables", "search": "needle%_ 00"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var joined map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &joined)
	if len(joined["entries"].([]any)) != 2 {
		t.Fatalf("site/pending declaration join duplicated rows: %s", nodes[0].Payload)
	}
	if len(nodes[0].Payload) > 8*1024 || strings.Contains(string(nodes[0].Payload), "large-plan") || strings.Contains(string(nodes[0].Payload), "private-settings") || strings.Contains(string(nodes[0].Payload), "private-build-log") {
		t.Fatalf("directory leaked detail payloads: %d bytes", len(nodes[0].Payload))
	}
	// A second engine reads the same stored definition through the same gate.
	second, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	second.Logger = discardLogger()
	if err = second.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	nodes, err = NewIntegration(second, discardLogger()).handleDirectoryPage(ctx, map[string]any{"resource": "sources"}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("replica read: %v", err)
	}
}
