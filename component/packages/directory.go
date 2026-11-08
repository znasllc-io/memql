package packages

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type directoryCursor struct {
	Version int    `json:"v"`
	AsOf    string `json:"asOf"`
	Key     string `json:"key"`
	AfterAt string `json:"afterAt"`
	AfterID string `json:"afterId"`
}
type directoryItem struct {
	key, app, name, hostname, kind, at, source, account string
	site, pkg, run                                      map[string]any
	disabled, archived                                  bool
}

func directoryText(row map[string]any, key string) string { s, _ := row[key].(string); return s }
func directoryBool(row map[string]any, key string) bool   { b, _ := row[key].(bool); return b }
func directoryRows(value any) []map[string]any {
	out := []map[string]any{}
	if rows, ok := value.([]any); ok {
		for _, v := range rows {
			if row, ok := v.(map[string]any); ok {
				out = append(out, row)
			}
		}
	}
	return out
}
func directoryStrings(value any) []string {
	out := []string{}
	if values, ok := value.([]any); ok {
		for _, v := range values {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	if values, ok := value.([]string); ok {
		return values
	}
	return out
}
func directoryHas(values []string, value string) bool {
	for _, s := range values {
		if s == value {
			return true
		}
	}
	return false
}
func directoryBare(id string) string { parts := strings.Split(id, ":"); return parts[len(parts)-1] }
func directoryClock(row map[string]any) string {
	if s := directoryText(row, "createdAt"); s != "" {
		if stamp, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return stamp.UTC().Format("2006-01-02T15:04:05.000000000Z")
		}
	}
	return ""
}
func directoryNewer(a, b map[string]any) bool {
	at, bt := directoryText(a, "startedAt"), directoryText(b, "startedAt")
	if at == "" {
		at = directoryText(a, "createdAt")
	}
	if bt == "" {
		bt = directoryText(b, "createdAt")
	}
	if stamp, err := time.Parse(time.RFC3339Nano, at); err == nil {
		at = stamp.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	if stamp, err := time.Parse(time.RFC3339Nano, bt); err == nil {
		bt = stamp.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	return at > bt || at == bt && directoryText(a, "id") > directoryText(b, "id")
}
func directoryCovers(run map[string]any, app string) bool {
	if run == nil {
		return false
	}
	scoped := directoryStrings(run["scopedTo"])
	return len(scoped) == 0 || directoryHas(scoped, app)
}
func directoryReportApps(run map[string]any) []map[string]any {
	report, _ := run["report"].(map[string]any)
	return directoryRows(report["deployables"])
}
func directorySource(pkg, site map[string]any) string {
	if pkg != nil {
		switch directoryText(pkg, "sourceKind") {
		case "repo":
			return "repository"
		case "artifact":
			return "zip"
		}
		return ""
	}
	if directoryText(site, "artifactId") != "" {
		return "zip"
	}
	ref := strings.TrimSpace(directoryText(site, "bundleRef"))
	if strings.HasPrefix(ref, "blob://sites/") || strings.HasSuffix(strings.TrimRight(ref, "/"), "/pending") {
		return "ci"
	}
	return ""
}
func directoryPackageSummary(pkg map[string]any) map[string]any {
	if pkg == nil {
		return nil
	}
	out := map[string]any{}
	for key, value := range pkg {
		if key != "declares" && key != "disabledDeployables" {
			out[key] = value
		}
	}
	return out
}

// Directory rows never carry full reports, candidates, or deployment logs.
// Opening an item separately requests the existing authorized detail feeds.
func directoryRunSummary(run map[string]any) map[string]any {
	if run == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"id", "createdAt", "packageId", "ownerUserId", "status", "startedAt", "requestedBy", "scopedTo"} {
		if value, ok := run[key]; ok {
			out[key] = value
		}
	}
	return out
}
func directoryName(row map[string]any) string {
	for _, key := range []string{"name", "repoUrl", "id"} {
		if s := strings.TrimSpace(directoryText(row, key)); s != "" {
			return s
		}
	}
	return ""
}

// Read identities in bounded authorized pages. No service actor, SQL bypass,
// or app-specific authorization approximation is used by this reducer.
func (i *Integration) directoryRead(ctx context.Context, call string) ([]map[string]any, error) {
	rows := []map[string]any{}
	err := memql.WalkQueryPages(ctx, i.engine.Execute, call, 10000, func(result *memql.ExecuteResult) error {
		rows = append(rows, memql.MaterializeRows(result.OutputPayload())...)
		return nil
	})
	return rows, err
}
func directoryCollect(sites, packages, runs []map[string]any) ([]directoryItem, map[string][]directoryItem) {
	pkgs := map[string]map[string]any{}
	pending := map[string]map[string]any{}
	served := map[string]bool{}
	groups := map[string][]directoryItem{}
	all := []directoryItem{}
	for _, pkg := range packages {
		pkgs[directoryBare(directoryText(pkg, "id"))] = pkg
	}
	for _, run := range runs {
		id := directoryBare(directoryText(run, "packageId"))
		if old := pending[id]; old == nil || directoryNewer(run, old) {
			pending[id] = run
		}
	}
	add := func(item directoryItem) {
		item.source = directorySource(item.pkg, item.site)
		item.account = directoryText(item.site, "accountId")
		if item.site == nil {
			item.account = directoryText(item.pkg, "accountId")
		}
		item.archived = directoryText(item.site, "status") == "archived" || directoryText(item.pkg, "status") == "archived"
		all = append(all, item)
		if item.pkg != nil {
			id := directoryText(item.pkg, "id")
			groups[id] = append(groups[id], item)
		}
	}
	for _, site := range sites {
		pkg := pkgs[directoryBare(directoryText(site, "packageId"))]
		app := ""
		var run map[string]any
		name := strings.TrimSpace(directoryText(site, "title"))
		if name == "" {
			name = directoryText(site, "hostname")
		}
		if name == "" {
			name = directoryText(site, "id")
		}
		if pkg != nil {
			app = directoryText(site, "packageDeployableName")
			served[directoryText(pkg, "id")+"/"+app] = true
			if app != "" {
				name = app
			}
			for _, declared := range directoryRows(pkg["declares"]) {
				if directoryText(declared, "name") == app && strings.TrimSpace(directoryText(declared, "displayName")) != "" {
					name = strings.TrimSpace(directoryText(declared, "displayName"))
				}
			}
			if candidate := pending[directoryBare(directoryText(pkg, "id"))]; directoryCovers(candidate, app) {
				run = candidate
			}
		}
		add(directoryItem{key: directoryText(site, "id"), app: app, name: name, hostname: directoryText(site, "hostname"), kind: directoryText(site, "kind"), at: directoryClock(site), site: site, pkg: pkg, run: run})
	}
	for _, pkg := range packages {
		id := directoryText(pkg, "id")
		run := pending[directoryBare(id)]
		if run != nil {
			apps := []map[string]any{}
			for _, app := range directoryReportApps(run) {
				if directoryCovers(run, directoryText(app, "name")) {
					apps = append(apps, app)
				}
			}
			if len(apps) == 0 {
				apps = append(apps, map[string]any{"name": "", "kind": ""})
			}
			for _, app := range apps {
				name := directoryText(app, "name")
				key := id + "/" + name
				if name != "" && served[key] {
					continue
				}
				served[key] = true
				label := strings.TrimSpace(directoryText(app, "displayName"))
				if label == "" {
					label = name
				}
				if label == "" {
					label = directoryName(pkg)
				}
				add(directoryItem{key: key, app: name, name: label, kind: directoryText(app, "kind"), at: directoryClock(run), pkg: pkg, run: run})
			}
		}
		for _, declared := range directoryRows(pkg["declares"]) {
			app := directoryText(declared, "name")
			key := id + "/" + app
			if app == "" || served[key] {
				continue
			}
			served[key] = true
			name := strings.TrimSpace(directoryText(declared, "displayName"))
			if name == "" {
				name = app
			}
			add(directoryItem{key: key, app: app, name: name, kind: directoryText(declared, "kind"), at: directoryClock(pkg), pkg: pkg, disabled: directoryHas(directoryStrings(pkg["disabledDeployables"]), app)})
		}
		if len(groups[id]) == 0 && !directoryBool(pkg, "sourceRemoved") && directoryText(pkg, "status") != "archived" {
			add(directoryItem{key: id + "/", name: directoryName(pkg), at: directoryClock(pkg), pkg: pkg})
		}
	}
	return all, groups
}
func directoryMatches(item directoryItem, args map[string]any) bool {
	if item.archived != directoryBool(args, "archived") {
		return false
	}
	if kind := directoryText(args, "kind"); kind != "" && item.kind != kind {
		return false
	}
	if status := directoryText(args, "status"); status != "" && directoryText(item.site, "status") != status {
		return false
	}
	if source := directoryText(args, "source"); source != "" && item.source != source {
		return false
	}
	if account := directoryText(args, "accountId"); account != "" && account != "all" {
		if account == "none" {
			if item.account != "" {
				return false
			}
		} else if directoryBare(item.account) != directoryBare(account) {
			return false
		}
	}
	needle := strings.ToLower(strings.TrimSpace(directoryText(args, "search")))
	return needle == "" || strings.Contains(strings.ToLower(strings.Join([]string{item.name, item.hostname, item.app, directoryText(item.pkg, "name"), directoryText(item.pkg, "repoUrl"), directoryText(item.pkg, "repoRef")}, " ")), needle)
}
func directoryProvenance(pkg map[string]any, credentials, connections []map[string]any) string {
	repo := strings.TrimSuffix(strings.TrimSuffix(directoryText(pkg, "repoUrl"), "/"), ".git")
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "https://github.com/"), "git@github.com:")
	target := strings.Split(repo, "/")[0]
	if target == "" {
		target = "Unknown target"
	}
	identity := "Unknown GitHub identity"
	for _, credential := range credentials {
		if directoryBare(directoryText(credential, "id")) == directoryBare(directoryText(pkg, "credentialId")) && directoryBare(directoryText(credential, "ownerUserId")) == directoryBare(directoryText(pkg, "ownerUserId")) && directoryText(credential, "kind") == "github_app" && directoryText(credential, "login") != "" {
			identity = "@" + directoryText(credential, "login")
		}
	}
	for _, connection := range connections {
		if directoryBare(directoryText(connection, "id")) == directoryBare(directoryText(pkg, "sourceConnectionId")) && directoryBare(directoryText(connection, "credentialId")) == directoryBare(directoryText(pkg, "credentialId")) && directoryBare(directoryText(connection, "ownerUserId")) == directoryBare(directoryText(pkg, "ownerUserId")) && directoryText(connection, "accountLogin") != "" {
			target = directoryText(connection, "accountLogin")
		}
	}
	ref := directoryText(pkg, "repoRef")
	if ref == "" {
		ref = "default branch"
	}
	return identity + " · " + target + " · " + repo + " · " + ref
}

func (i *Integration) handleDirectoryPage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, _ := auth.AccessFromContext(ctx)
	if actor == nil || actor.UserId == "" {
		return nil, fmt.Errorf("deployable directory requires a signed-in caller")
	}
	if i.engine == nil {
		return nil, fmt.Errorf("deployable directory engine is unavailable")
	}
	resource := directoryText(args, "resource")
	if resource == "" {
		resource = "deployables"
	}
	if resource != "deployables" && resource != "sources" {
		return nil, fmt.Errorf("unknown deployable directory")
	}
	question := map[string]any{}
	for _, key := range []string{"resource", "search", "kind", "status", "accountId", "source", "archived"} {
		question[key] = args[key]
	}
	encoded, _ := json.Marshal(question)
	sum := sha256.Sum256(encoded)
	key := hex.EncodeToString(sum[:])
	cursor := directoryCursor{Version: 1, AsOf: time.Now().UTC().Format(time.RFC3339Nano), Key: key}
	if raw := directoryText(args, "cursor"); raw != "" {
		if len(raw) > 4096 {
			return nil, fmt.Errorf("invalid directory cursor")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.Version != 1 || cursor.Key != key || cursor.AfterID == "" {
			return nil, fmt.Errorf("invalid directory cursor")
		}
		at, err := time.Parse(time.RFC3339Nano, cursor.AsOf)
		if err != nil || at.After(time.Now().Add(time.Minute)) {
			return nil, fmt.Errorf("invalid directory snapshot time")
		}
	}
	asOf, _ := json.Marshal(cursor.AsOf)
	sites, err := i.directoryRead(ctx, fmt.Sprintf("query deployableDirectorySites(asOf: %s)", asOf))
	if err != nil {
		return nil, err
	}
	packages, err := i.directoryRead(ctx, fmt.Sprintf("query deployableDirectoryPackages(asOf: %s)", asOf))
	if err != nil {
		return nil, err
	}
	runs, err := i.directoryRead(ctx, fmt.Sprintf("query deployableDirectoryPending(asOf: %s)", asOf))
	if err != nil {
		return nil, err
	}
	all, groups := directoryCollect(sites, packages, runs)
	items := []directoryItem{}
	archivedCount := 0
	provenance := map[string]string{}
	summaries := map[string]map[string]any{}
	states := map[string]map[string]any{}
	provenanceError := ""
	if resource == "sources" {
		// Personal provenance is optional naming data. A caller unable to read a
		// personal grant still sees authorized configured sources with honest unknown labels.
		credentials, credentialErr := i.directoryRead(ctx, "query deployableDirectoryCredentials()")
		connections, connectionErr := i.directoryRead(ctx, "query deployableDirectoryConnections()")
		if credentialErr != nil || connectionErr != nil {
			provenanceError = "Some GitHub provenance could not be refreshed."
		}
		needle := strings.ToLower(strings.TrimSpace(directoryText(args, "search")))
		for _, pkg := range packages {
			if directoryBool(pkg, "sourceRemoved") || directoryText(pkg, "sourceKind") != "repo" || directoryText(pkg, "repoUrl") == "" {
				continue
			}
			archived := directoryText(pkg, "status") == "archived"
			if archived {
				archivedCount++
			}
			if archived != directoryBool(args, "archived") {
				continue
			}
			id := directoryText(pkg, "id")
			rows := groups[id]
			prov := directoryProvenance(pkg, credentials, connections)
			hay := directoryName(pkg) + " " + prov
			apps, deployed, waiting, analyzing := 0, 0, false, false
			for _, item := range rows {
				hay += " " + item.name + " " + item.hostname
				if item.app != "" || item.site != nil {
					apps++
				}
				if item.site != nil && directoryText(item.site, "status") != "archived" {
					deployed++
				}
				waiting = waiting || directoryText(item.run, "status") == "awaiting_confirm"
				analyzing = analyzing || directoryText(item.run, "status") == "analyzing"
			}
			if needle != "" && !strings.Contains(strings.ToLower(hay), needle) {
				continue
			}
			word, tone := "Current", "muted"
			if deployed == 0 {
				word = "Nothing deployed"
			}
			if directoryBool(pkg, "updateAvailable") {
				word = "Update available"
				tone = "accent"
			}
			if waiting {
				word = "Review needed"
				tone = "warn"
			}
			if analyzing {
				word = "Analyzing"
				tone = "muted"
			}
			if archived {
				word = "Archived"
				tone = "muted"
			}
			provenance[id] = prov
			summaries[id] = map[string]any{"apps": apps, "deployed": deployed, "waiting": waiting}
			states[id] = map[string]any{"word": word, "tone": tone}
			items = append(items, directoryItem{key: "pkg:" + id, at: directoryClock(pkg), pkg: pkg})
		}
	} else {
		for _, item := range all {
			if item.archived {
				archivedCount++
			}
			if directoryMatches(item, args) {
				items = append(items, item)
			}
		}
	}
	sort.Slice(items, func(a, b int) bool {
		if items[a].at != items[b].at {
			return items[a].at > items[b].at
		}
		return items[a].key < items[b].key
	})
	total := len(items)
	page := []directoryItem{}
	for _, item := range items {
		if cursor.AfterID != "" && (item.at > cursor.AfterAt || item.at == cursor.AfterAt && item.key <= cursor.AfterID) {
			continue
		}
		page = append(page, item)
		if len(page) == 11 {
			break
		}
	}
	more := len(page) > 10
	if more {
		page = page[:10]
	}
	// Full site hydration is a single bounded batch, only for visible entries.
	ids := []string{}
	for _, item := range page {
		if item.site != nil {
			ids = append(ids, directoryText(item.site, "id"))
		}
	}
	hydrated := map[string]map[string]any{}
	if len(ids) > 0 {
		raw, _ := json.Marshal(ids)
		rows, err := i.directoryRead(ctx, fmt.Sprintf("query deployableDirectorySitesByIds(ids: %s)", raw))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			hydrated[directoryText(row, "id")] = row
		}
	}
	entries := []map[string]any{}
	for _, item := range page {
		entry := map[string]any{"id": item.key, "createdAt": item.at}
		if resource == "sources" {
			id := directoryText(item.pkg, "id")
			entry["group"] = map[string]any{"id": item.key, "pkg": directoryPackageSummary(item.pkg), "rows": []any{}, "section": "source", "startsSection": false, "summary": summaries[id], "state": states[id], "provenance": provenance[id]}
		}
		if resource == "deployables" {
			site := item.site
			if site != nil {
				site = hydrated[directoryText(site, "id")]
			}
			owner := directoryText(item.run, "requestedBy")
			if owner == "" {
				owner = directoryText(site, "ownerUserId")
			}
			if owner == "" {
				owner = directoryText(item.pkg, "ownerUserId")
			}
			entry["row"] = map[string]any{"key": item.key, "site": site, "pkg": directoryPackageSummary(item.pkg), "app": item.app, "name": item.name, "hostname": item.hostname, "kind": item.kind, "parked": directoryRunSummary(item.run), "deployedBy": directoryBare(owner), "disabled": item.disabled}
		}
		entries = append(entries, entry)
	}
	next := ""
	if more {
		last := page[len(page)-1]
		cursor.AfterAt = last.at
		cursor.AfterID = last.key
		raw, _ := json.Marshal(cursor)
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	payload, _ := json.Marshal(map[string]any{"entries": entries, "nextCursor": next, "total": total, "archivedCount": archivedCount, "provenanceError": provenanceError})
	return []memorynodes.MemoryNode{{ID: "deployable-directory", Concept: "integration:packages:directory", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
