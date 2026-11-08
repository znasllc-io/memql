package email

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
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
)

const inboxPageSize = 10
const inboxScanBudget = 100

var inboxCursorIDs = id.NewUntracked()

type inboxCursor struct {
	Version   int    `json:"v"`
	AsOf      string `json:"asOf"`
	Position  string `json:"position"`
	Offset    int    `json:"offset"`
	SearchKey string `json:"key"`
}

type capturedSummary struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
	AccountID string `json:"accountId,omitempty"`
	From      string `json:"from"`
	FromName  string `json:"fromName"`
	To        string `json:"To"`
	Subject   string `json:"Subject"`
}

func inboxAuthorized(ctx context.Context) error {
	if err := configureAuthorized(ctx); err != nil {
		return err
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac.Synthetic {
		return fmt.Errorf("email: test inbox requires a signed-in operator")
	}
	return nil
}

func inboxSearchKey(search, account string) string {
	bound, _ := json.Marshal([]string{search, account})
	return string(inboxCursorIDs.FromBytes(bound))
}

func decodeInboxCursor(raw, key string, now time.Time) (inboxCursor, error) {
	fail := fmt.Errorf("email: this inbox page is no longer available; read the inbox again")
	if len(raw) > 2048 {
		return inboxCursor{}, fail
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	var cursor inboxCursor
	if err != nil || json.Unmarshal(body, &cursor) != nil || cursor.Version != 1 || cursor.SearchKey != key || cursor.Offset < 0 || cursor.Offset >= inboxPageSize {
		return inboxCursor{}, fail
	}
	asOf, err := time.Parse(time.RFC3339Nano, cursor.AsOf)
	if err != nil || asOf.After(now.Add(time.Minute)) {
		return inboxCursor{}, fail
	}
	return cursor, nil
}

func encodeInboxCursor(cursor inboxCursor) string {
	body, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(body)
}

func decryptCaptured(row map[string]any) (capturedMessage, error) {
	var item capturedMessage
	sealed, _ := row["encryptedValue"].(string)
	plain, err := secret.Decrypt(sealed)
	if err != nil || json.Unmarshal([]byte(plain), &item) != nil {
		return item, fmt.Errorf("email: could not read captured message")
	}
	return item, nil
}

// The payload is encrypted as a whole. Search therefore decrypts bounded pages
// on the server, never sends bodies to a directory, and carries its scan frontier
// even when the next ten matches take more than one request to find.
func (i *Integration) handleInboxPage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := inboxAuthorized(ctx); err != nil {
		return nil, err
	}
	sender, capture := captureSender(i.sender)
	if !capture {
		return configureResult(map[string]any{"mode": "external", "messages": []capturedSummary{}, "nextCursor": ""})
	}
	if sender.store == nil {
		return nil, fmt.Errorf("email: capture storage is unavailable")
	}
	search, _ := args["search"].(string)
	search = strings.ToLower(strings.TrimSpace(search))
	account, _ := args["accountId"].(string)
	raw, _ := args["cursor"].(string)
	now := time.Now().UTC()
	cursor := inboxCursor{Version: 1, AsOf: now.Format(time.RFC3339Nano), SearchKey: inboxSearchKey(search, account)}
	if raw != "" {
		var err error
		cursor, err = decodeInboxCursor(raw, cursor.SearchKey, now)
		if err != nil {
			return nil, err
		}
	}
	asOf, _ := time.Parse(time.RFC3339Nano, cursor.AsOf)
	sinceTime := asOf.Add(-7 * 24 * time.Hour)
	if currentWindow := now.Add(-7 * 24 * time.Hour); sinceTime.Before(currentWindow) {
		sinceTime = currentWindow
	}
	since := sinceTime.Format(time.RFC3339Nano)
	items := make([]capturedSummary, 0, inboxPageSize)
	more := false
	scanned := 0
	for scanned < inboxScanBudget && len(items) < inboxPageSize {
		queryArgs := map[string]string{"since": since, "asOf": cursor.AsOf}
		result, err := sender.store.Execute(memql.ContextWithCursor(captureContext(ctx), cursor.Position), strings.Replace(renderConfigCall("capturedEmailsPage", queryArgs), "mutation ", "query ", 1))
		if err != nil {
			return nil, fmt.Errorf("email: could not read captured messages")
		}
		executed, ok := result.(*memql.ExecuteResult)
		if !ok {
			return nil, fmt.Errorf("email: captured page returned no continuation metadata")
		}
		rows := memql.MaterializeRows(executed.OutputPayload())
		next := ""
		if executed.GetMeta() != nil {
			next = executed.GetMeta().Cursor
		}
		more = next != ""
		start := cursor.Offset
		if start > len(rows) {
			return configureResult(map[string]any{"mode": "capture", "messages": items, "nextCursor": ""})
		}
		for index := start; index < len(rows); index++ {
			row := rows[index]
			item, err := decryptCaptured(row)
			if err != nil {
				return nil, err
			}
			scanned++
			matches := account == "" || item.AccountID == account
			if matches && search != "" {
				matches = strings.Contains(strings.ToLower(item.Subject+" "+item.To+" "+item.From+" "+item.FromName), search)
			}
			if matches {
				items = append(items, capturedSummary{ID: item.ID, CreatedAt: item.CreatedAt, AccountID: item.AccountID, From: item.From, FromName: item.FromName, To: item.To, Subject: item.Subject})
			}
			cursor.Offset = index + 1
			if len(items) == inboxPageSize || scanned == inboxScanBudget {
				break
			}
		}
		if cursor.Offset == len(rows) {
			cursor.Position = next
			cursor.Offset = 0
		} else {
			more = true
		}

		if !more {
			break
		}
	}
	next := ""
	if more {
		next = encodeInboxCursor(cursor)
	}
	return configureResult(map[string]any{"mode": "capture", "messages": items, "nextCursor": next})
}

func (i *Integration) handleInboxMessage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := inboxAuthorized(ctx); err != nil {
		return nil, err
	}
	sender, capture := captureSender(i.sender)
	if !capture {
		return configureResult(map[string]any{"message": nil})
	}
	if sender.store == nil {
		return nil, fmt.Errorf("email: capture storage is unavailable")
	}
	id, _ := args["messageId"].(string)
	if id == "" || len(id) > 256 {
		return nil, fmt.Errorf("email: choose a captured message")
	}
	result, err := sender.store.Execute(captureContext(ctx), strings.Replace(renderConfigCall("capturedEmailById", map[string]string{"messageId": id, "since": time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)}), "mutation ", "query ", 1))
	if err != nil {
		return nil, fmt.Errorf("email: could not read captured message")
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		return configureResult(map[string]any{"message": nil})
	}
	item, err := decryptCaptured(rows[0])
	if err != nil {
		return nil, err
	}
	return configureResult(map[string]any{"message": item})
}
