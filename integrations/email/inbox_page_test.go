package email

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

type inboxPageStore struct {
	rows    []map[string]any
	reads   int
	largest int
}

func (s *inboxPageStore) Execute(ctx context.Context, query string) (any, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || !ac.Synthetic || !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("not internal")
	}
	s.reads++
	args := map[string]string{}
	for _, match := range regexp.MustCompile(`(\w+): ("[^"\\]*(?:\\.[^"\\]*)*")`).FindAllStringSubmatch(query, -1) {
		args[match[1]], _ = strconv.Unquote(match[2])
	}
	result := []map[string]any{}
	for _, row := range s.rows {
		stamp, id := row["createdAt"].(string), row["id"].(string)
		if stamp < args["since"] || args["asOf"] != "" && stamp > args["asOf"] {
			continue
		}
		if args["messageId"] != "" && id != args["messageId"] {
			continue
		}
		if args["before"] != "" && (stamp > args["before"] || stamp == args["before"] && id <= args["beforeId"]) {
			continue
		}
		result = append(result, row)
	}
	offset, _ := strconv.Atoi(memql.CursorFromContext(ctx))
	next := ""
	if offset > len(result) {
		return nil, fmt.Errorf("invalid cursor")
	}
	result = result[offset:]
	if len(result) > 10 {
		result = result[:10]
		next = strconv.Itoa(offset + 10)
	}
	if len(result) > s.largest {
		s.largest = len(result)
	}
	response := memql.NewResultWithOutput(result)
	response.SetCursor(next)
	return response, nil
}
func inboxFixture(t *testing.T, count int) *inboxPageStore {
	t.Helper()
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ab", 32))
	store := &inboxPageStore{}
	now := time.Now().UTC().Add(-time.Minute)
	for n := 0; n < count; n++ {
		stamp := now.Add(-time.Duration(n/2) * time.Second).Format(time.RFC3339Nano)
		account := "acme"
		if n%2 == 1 {
			account = "other"
		}
		item := capturedMessage{ID: fmt.Sprintf("m%03d", n), CreatedAt: stamp, AccountID: account, From: "news@acme.test", FromName: "Acme", Message: Message{To: "person@example.test", Subject: fmt.Sprintf("Message %03d", n), TextBody: strings.Repeat("private-body-", 1200), HTMLBody: "<p>secret-token</p>"}}
		body, _ := json.Marshal(item)
		sealed, _, err := secret.Encrypt(string(body))
		if err != nil {
			t.Fatal(err)
		}
		store.rows = append(store.rows, map[string]any{"id": item.ID, "createdAt": stamp, "encryptedValue": sealed})
	}
	sort.Slice(store.rows, func(a, b int) bool {
		x, y := store.rows[a], store.rows[b]
		if x["createdAt"] == y["createdAt"] {
			return x["id"].(string) < y["id"].(string)
		}
		return x["createdAt"].(string) > y["createdAt"].(string)
	})
	return store
}
func inboxPayload(t *testing.T, rows []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	var body map[string]any
	if err := json.Unmarshal(rows[0].Payload, &body); err != nil {
		t.Fatal(err)
	}
	return body
}
func TestInboxPageBudgetsCursorSearchAndDetail(t *testing.T) {
	store := inboxFixture(t, 125)
	integration := NewIntegration(&CaptureSender{store: store}, nil)
	ctx := configureCtx(auth.RoleDeveloper)
	rows, err := integration.handleInboxPage(ctx, nil, 0)
	first := inboxPayload(t, rows, err)
	if len(first["messages"].([]any)) != 10 || store.reads != 1 || store.largest != 10 {
		t.Fatalf("unbounded first page: reads=%d result=%v", store.reads, first)
	}
	encoded, _ := json.Marshal(first)
	if len(encoded) > 4096 || strings.Contains(string(encoded), "private-body") || strings.Contains(string(encoded), "TextBody") || strings.Contains(string(encoded), "Headers") {
		t.Fatalf("directory leaked heavy content: %d bytes", len(encoded))
	}
	rows, err = integration.handleInboxPage(ctx, map[string]any{"cursor": first["nextCursor"]}, 0)
	second := inboxPayload(t, rows, err)
	if second["messages"].([]any)[0].(map[string]any)["id"] != "m010" {
		t.Fatal("tie cursor skipped or repeated a message")
	}
	rows, err = integration.handleInboxMessage(ctx, map[string]any{"messageId": "m124"}, 0)
	detail := inboxPayload(t, rows, err)
	if !strings.Contains(detail["message"].(map[string]any)["TextBody"].(string), "private-body") {
		t.Fatal("direct detail lost body")
	}
	store.reads = 0
	rows, err = integration.handleInboxPage(ctx, map[string]any{"search": "MESSAGE 124"}, 0)
	sparse := inboxPayload(t, rows, err)
	if len(sparse["messages"].([]any)) != 0 || sparse["nextCursor"] == "" || store.reads != 10 {
		t.Fatalf("sparse page failed bounded continuation: %#v reads=%d", sparse, store.reads)
	}
	rows, err = integration.handleInboxPage(ctx, map[string]any{"search": "MESSAGE 124", "cursor": sparse["nextCursor"]}, 0)
	found := inboxPayload(t, rows, err)
	if len(found["messages"].([]any)) != 1 || found["nextCursor"] != "" {
		t.Fatalf("sparse continuation lost match: %#v", found)
	}
	rows, err = integration.handleInboxPage(ctx, map[string]any{"accountId": "other"}, 0)
	scoped := inboxPayload(t, rows, err)
	for _, raw := range scoped["messages"].([]any) {
		if raw.(map[string]any)["accountId"] != "other" {
			t.Fatal("organization filter leaked another organization's message")
		}
	}
	for _, args := range []map[string]any{{"cursor": "bad"}, {"cursor": first["nextCursor"], "search": "changed"}, {"cursor": first["nextCursor"], "accountId": "other"}} {
		reads := store.reads
		if _, err := integration.handleInboxPage(ctx, args, 0); err == nil || store.reads != reads {
			t.Fatal("invalid or mismatched cursor reached storage")
		}
	}
}
func TestInboxPagesAuthorizeBeforeStorage(t *testing.T) {
	store := inboxFixture(t, 1)
	integration := NewIntegration(&CaptureSender{store: store}, nil)
	for _, ctx := range []context.Context{context.Background(), configureCtx(auth.RoleAdmin), configureCtx(auth.RoleWriter), configureCtx(auth.RoleReader), auth.ContextWithAccess(context.Background(), auth.SystemActor("test"))} {
		if _, err := integration.handleInboxPage(ctx, nil, 0); err == nil {
			t.Fatal("unauthorized directory")
		}
		if _, err := integration.handleInboxMessage(ctx, map[string]any{"messageId": "m000"}, 0); err == nil {
			t.Fatal("unauthorized detail")
		}
	}
	if store.reads != 0 {
		t.Fatal("refused caller touched encrypted storage")
	}
}
