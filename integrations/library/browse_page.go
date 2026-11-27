package library

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

type directoryReader interface {
	LibraryDirectoryRows(context.Context, string, map[string]any) ([]map[string]any, error)
}

// Hash-only use: do not retain owners, filters or intermediate IDs.
var directoryScopeIDs = id.NewUntracked()

func (i *Integration) directoryRows(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	reader, ok := i.engine.(directoryReader)
	if !ok {
		return nil, fmt.Errorf("library directory reader is unavailable")
	}
	return reader.LibraryDirectoryRows(ctx, name, args)
}

// The ordinary chronological list remains a bounded engine query. This read
// serves the orders and derived file filters that used to make the browser
// drain every ten-row page. Reduce once on the server and send just the page.
// No result population or cursor state is kept on a replica. Every continuation
// applies current authorization and the same scope, across nodes and restarts.
func (i *Integration) handleBrowsePage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := auth.AccessFromContext(ctx)
	if access == nil || access.UserId == "" {
		return nil, fmt.Errorf("library directory requires an authenticated caller")
	}
	order := asString(args["sort"])
	if order == "" {
		order = "newest"
	}
	if order != "name" && order != "kind" && order != "size" && order != "newest" && order != "oldest" {
		return nil, fmt.Errorf("invalid library sort")
	}
	kind := asString(args["fileType"])
	if kind == "" {
		kind = "all"
	}
	if !validDirectoryKind(kind) {
		return nil, fmt.Errorf("invalid library file type")
	}
	locale := asString(args["locale"])
	if locale == "" {
		locale = "en"
	}
	if len(locale) > 80 {
		return nil, fmt.Errorf("invalid library locale")
	}
	tag, err := language.Parse(locale)
	if err != nil {
		return nil, fmt.Errorf("invalid library locale")
	}
	var since time.Time
	if value := asString(args["since"]); value != "" {
		since, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("invalid library date filter")
		}
	}
	compare := directoryComparator(order, collate.New(tag, collate.IgnoreCase, collate.IgnoreDiacritics, collate.Numeric))
	scope := map[string]any{"owner": access.UserId}
	filters := map[string]any{}
	for _, key := range []string{"archived", "folderId", "kind", "source", "accountId", "label", "search", "ids", "folderIds", "sourceRefs"} {
		if value, ok := args[key]; ok && value != nil {
			filters[key], scope[key] = value, value
		}
	}
	for _, key := range []string{"sort", "fileType", "since", "locale"} {
		scope[key] = args[key]
	}
	scopeID, err := directoryScopeIDs.FromMap(scope)
	if err != nil {
		return nil, fmt.Errorf("invalid library filters: %w", err)
	}
	fingerprint := string(scopeID)
	cursor := directoryCursor{Version: 1, Scope: fingerprint, AsOf: time.Now().UTC()}
	if token := asString(args["cursor"]); token != "" {
		if len(token) > 16384 {
			return nil, fmt.Errorf("invalid library cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Scope != fingerprint || cursor.After.ID == "" || cursor.AsOf.IsZero() || cursor.AsOf.After(time.Now().Add(time.Minute)) {
			return nil, fmt.Errorf("invalid library cursor; reopen this folder")
		}
	}
	filters["asOf"] = cursor.AsOf.Format(time.RFC3339Nano)
	rows, err := i.directoryRows(ctx, "libraryArtifactsBrowse", filters)
	if err != nil {
		return nil, err
	}
	var files map[string]float64
	if order == "size" && len(rows) > 0 {
		sizes, err := i.directoryRows(ctx, "libraryFileSizeRows", nil)
		if err != nil {
			return nil, err
		}
		files = map[string]float64{}
		for _, file := range sizes {
			if size, ok := file["size"].(float64); ok {
				files[memql.BareShortId(asString(file["id"]))] = size
			}
		}
	}
	items := make([]directoryItem, 0, len(rows))
	for _, row := range rows {
		at, err := time.Parse(time.RFC3339Nano, timestampField(row, "createdAt"))
		if !since.IsZero() && (err != nil || at.Before(since)) {
			continue
		}
		fileKind := directoryFileKind(row)
		if kind != "all" && kind != fileKind {
			continue
		}
		id := asString(row["id"])
		name := strings.TrimSpace(asString(row["title"]))
		if name == "" {
			name = id
		}
		item := directoryItem{ID: id, Name: name, Kind: fileKind, At: at, Size: -1, Row: row}
		if asString(row["kind"]) == "file" {
			if size, ok := files[memql.BareShortId(asString(row["sourceConceptRef"]))]; ok {
				item.Size = size
				row["browseSize"] = size
			}
		}
		if cursor.After.ID != "" && compare(item, cursor.After) <= 0 {
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(a, b int) bool { return compare(items[a], items[b]) < 0 })
	next := ""
	if len(items) > 10 {
		items = items[:10]
		cursor.After = items[9]
		cursor.After.Row = nil
		raw, _ := json.Marshal(cursor)
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	page := make([]map[string]any, 0, len(items))
	for _, item := range items {
		page = append(page, item.Row)
	}
	payload, err := json.Marshal(map[string]any{"rows": page, "cursor": next})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "library-page", Concept: resultConcept, Type: memorynodes.NodeTypeObject, Payload: payload}}, nil
}

type directoryItem struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	At   time.Time      `json:"at"`
	Size float64        `json:"size"`
	Row  map[string]any `json:"-"`
}
type directoryCursor struct {
	Version int           `json:"v"`
	Scope   string        `json:"scope"`
	AsOf    time.Time     `json:"asOf"`
	After   directoryItem `json:"after"`
}

func directoryComparator(order string, names *collate.Collator) func(directoryItem, directoryItem) int {
	return func(a, b directoryItem) int {
		if order == "newest" || order == "oldest" {
			c := a.At.Compare(b.At)
			if c == 0 {
				c = strings.Compare(a.ID, b.ID)
			}
			if order == "newest" {
				return -c
			}
			return c
		}
		if order == "size" && a.Size != b.Size {
			if a.Size > b.Size {
				return -1
			}
			return 1
		}
		if order == "kind" && a.Kind != b.Kind {
			return directoryKindRank(a.Kind) - directoryKindRank(b.Kind)
		}
		if c := names.CompareString(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	}
}
