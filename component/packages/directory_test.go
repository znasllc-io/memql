package packages

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type directoryTestEngine struct {
	t     *testing.T
	rows  map[string][]map[string]any
	calls map[string]int
	actor string
	fail  string
}

func (e *directoryTestEngine) Execute(ctx context.Context, call string) (*memql.ExecuteResult, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.UserId != e.actor || ac.Synthetic || auth.OriginFromContext(ctx).IsInternal() {
		e.t.Fatal("directory changed the caller's authority")
	}
	name := strings.Split(strings.TrimPrefix(call, "query "), "(")[0]
	e.calls[name]++
	if name == e.fail {
		return nil, fmt.Errorf("provenance unavailable")
	}
	rows := e.rows[name]
	if name == "deployableDirectorySitesByIds" {
		start := strings.Index(call, "[")
		end := strings.LastIndex(call, "]")
		var ids []string
		if start < 0 || json.Unmarshal([]byte(call[start:end+1]), &ids) != nil {
			return nil, fmt.Errorf("bad batch")
		}
		rows = nil
		for _, row := range e.rows["deployableDirectorySites"] {
			if directoryHas(ids, directoryText(row, "id")) {
				rows = append(rows, row)
			}
		}
	}
	result := memql.NewResultWithOutput(rows)
	return result, nil
}
func TestDirectoryDerivedRowsPreservePendingDeclaredAndSourceCounts(t *testing.T) {
	pkg := map[string]any{"id": "package", "name": "Repository", "sourceKind": "repo", "repoUrl": "https://github.com/acme/project", "createdAt": "2026-11-01T00:00:00Z", "declares": []any{map[string]any{"name": "live", "displayName": "Public shop", "kind": "spa"}, map[string]any{"name": "pending", "kind": "static"}, map[string]any{"name": "disabled", "kind": "spa"}}, "disabledDeployables": []any{"disabled"}}
	sites := []map[string]any{{"id": "site", "packageId": "package", "packageDeployableName": "live", "hostname": "shop.example.test", "kind": "spa", "status": "live", "createdAt": "2026-11-02T00:00:00Z"}}
	run := map[string]any{"id": "run", "packageId": "package", "status": "awaiting_confirm", "scopedTo": []any{"pending"}, "report": map[string]any{"deployables": []any{map[string]any{"name": "live"}, map[string]any{"name": "pending", "kind": "static"}}}, "createdAt": "2026-11-03T00:00:00Z"}
	rows, groups := directoryCollect(sites, []map[string]any{pkg}, []map[string]any{run})
	if len(rows) != 3 || len(groups["package"]) != 3 {
		t.Fatalf("lost/duplicated derived rows: %#v", rows)
	}
	byName := map[string]directoryItem{}
	for _, row := range rows {
		byName[row.app] = row
	}
	if byName["live"].name != "Public shop" || byName["live"].run != nil {
		t.Fatal("pending run tainted an unrelated live sibling")
	}
	if byName["pending"].run == nil || byName["pending"].disabled {
		t.Fatal("declared row hid the pending run")
	}
	if !byName["disabled"].disabled || byName["disabled"].site != nil {
		t.Fatal("disabled declaration disappeared")
	}
	if !directoryMatches(byName["live"], map[string]any{"search": "SHOP.EXAMPLE", "source": "repository"}) || directoryMatches(byName["disabled"], map[string]any{"status": "live"}) {
		t.Fatal("search/facet semantics changed")
	}
}
func TestDirectoryPagesSearchBeforeTenAndHydrateOneBatch(t *testing.T) {
	engine := &directoryTestEngine{t: t, rows: map[string][]map[string]any{}, calls: map[string]int{}, actor: "reader"}
	base := time.Now().UTC().Add(-time.Hour)
	for n := 0; n < 25; n++ {
		name := fmt.Sprintf("Recent %02d", n)
		if n < 12 {
			name = fmt.Sprintf("Earlier NEEDLE%%_ %02d", n)
		}
		engine.rows["deployableDirectorySites"] = append(engine.rows["deployableDirectorySites"], map[string]any{"id": fmt.Sprintf("site-%02d", n), "title": name, "hostname": fmt.Sprintf("site-%02d.example.test", n), "createdAt": base.Add(time.Duration(n/2) * time.Minute).Format(time.RFC3339Nano), "status": "live", "kind": "spa", "bundleRef": "blob://sites/live"})
	}
	integration := NewIntegration(engine, nil)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader", Role: auth.RoleWriter})
	walk := func(search string, sizes []int) {
		cursor := ""
		seen := map[string]bool{}
		for n, size := range sizes {
			nodes, err := integration.handleDirectoryPage(ctx, map[string]any{"resource": "deployables", "search": search, "cursor": cursor}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err = json.Unmarshal(nodes[0].Payload, &result); err != nil {
				t.Fatal(err)
			}
			rows := result["entries"].([]any)
			if len(rows) != size {
				t.Fatalf("page%d got%d want%d", n, len(rows), size)
			}
			for _, raw := range rows {
				id := raw.(map[string]any)["id"].(string)
				if seen[id] {
					t.Fatal("duplicate cursor row")
				}
				seen[id] = true
			}
			cursor = result["nextCursor"].(string)
			if (cursor != "") != (n < len(sizes)-1) {
				t.Fatal("wrong continuation")
			}
		}
	}
	walk("", []int{10, 10, 5})
	walk("needle%_", []int{10, 2})
	if engine.calls["deployableDirectorySitesByIds"] != 5 {
		t.Fatalf("hydration fanout: %#v", engine.calls)
	}
	reads := len(engine.calls)
	if _, err := integration.handleDirectoryPage(ctx, map[string]any{"cursor": "broken"}, 0); err == nil || len(engine.calls) != reads {
		t.Fatal("bad cursor executed")
	}
}

func TestDirectoryCanonicalReferencesAndPreciseRecentOrder(t *testing.T) {
	pkg := map[string]any{"id": "v1:platform:package:p", "ownerUserId": "v1:identity:user:u", "sourceKind": "repo", "repoUrl": "https://github.com/acme/project.git", "credentialId": "credential", "sourceConnectionId": "v1:platform:sourceConnection:connection", "declares": []any{map[string]any{"name": "web", "displayName": "Web"}}}
	site := map[string]any{"id": "site", "packageId": "p", "packageDeployableName": "web", "status": "live"}
	older := map[string]any{"id": "z", "packageId": "v1:platform:package:p", "startedAt": "2026-11-01T01:00:00+01:00"}
	newer := map[string]any{"id": "a", "packageId": "p", "startedAt": "2026-11-01T00:00:00.000000001Z"}
	rows, groups := directoryCollect([]map[string]any{site}, []map[string]any{pkg}, []map[string]any{newer, older})
	if len(groups["v1:platform:package:p"]) != 2 || rows[0].name != "Web" || directoryText(rows[0].run, "id") != "a" {
		t.Fatalf("canonical join or nanosecond ordering failed: %#v", rows)
	}
	credentials := []map[string]any{{"id": "v1:platform:sourceCredential:credential", "ownerUserId": "u", "kind": "github_app", "login": "owner"}}
	connections := []map[string]any{{"id": "connection", "credentialId": "v1:platform:sourceCredential:credential", "ownerUserId": "u", "accountLogin": "organization"}}
	if got := directoryProvenance(pkg, credentials, connections); got != "@owner · organization · acme/project · default branch" {
		t.Fatal(got)
	}
	connections[0]["ownerUserId"] = "someone-else"
	if strings.Contains(directoryProvenance(pkg, credentials, connections), "organization") {
		t.Fatal("foreign connection used for provenance")
	}
}

func TestDirectoryOldSessionCursorAndUnavailableProvenance(t *testing.T) {
	engine := &directoryTestEngine{t: t, rows: map[string][]map[string]any{}, calls: map[string]int{}, actor: "reader"}
	for n := 0; n < 11; n++ {
		engine.rows["deployableDirectoryPackages"] = append(engine.rows["deployableDirectoryPackages"], map[string]any{"id": fmt.Sprintf("p%02d", n), "sourceKind": "repo", "repoUrl": "https://github.com/acme/project", "createdAt": "2025-01-01T00:00:00Z"})
	}
	i := NewIntegration(engine, nil)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader", Role: auth.RoleWriter})
	nodes, err := i.handleDirectoryPage(ctx, map[string]any{"resource": "sources"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &first)
	raw, _ := base64.RawURLEncoding.DecodeString(first["nextCursor"].(string))
	var cursor directoryCursor
	_ = json.Unmarshal(raw, &cursor)
	cursor.AsOf = time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339Nano)
	raw, _ = json.Marshal(cursor)
	engine.fail = "deployableDirectoryConnections"
	nodes, err = i.handleDirectoryPage(ctx, map[string]any{"resource": "sources", "cursor": base64.RawURLEncoding.EncodeToString(raw)}, 0)
	if err != nil {
		t.Fatal("session cursor expired arbitrarily:", err)
	}
	var second map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &second)
	if len(second["entries"].([]any)) != 1 || second["provenanceError"] == "" {
		t.Fatalf("missing continuation or false complete provenance: %#v", second)
	}
}
