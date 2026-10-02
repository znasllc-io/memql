package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// The receipt is written BEFORE the network call. A submitting/unknown receipt
// can only be reconciled by GET; it never grants permission to repeat the POST.
// Content and credentials are deliberately absent. The digest binds a retry to
// the encoded message without retaining a second copy of its private contents.
type acsOperation struct {
	ID, AccountID, Endpoint, Digest, Intent string
	Status, Detail                          string
	SubmittedAt, CheckedAt, NextPollAt      time.Time
}

type acsOperationStore interface {
	lock(context.Context, string) (func(), error)
	read(context.Context, string) (acsOperation, time.Time, bool, error)
	write(context.Context, string, acsOperation, time.Time) (time.Time, error)
}

type storedACSOperations struct{ connection *connectionStore }

func (s *storedACSOperations) lock(ctx context.Context, key string) (func(), error) {
	var db *sql.DB
	if s != nil && s.connection != nil && s.connection.database != nil {
		db = s.connection.database()
	}
	return memql.AcquireWriteGate(ctx, db, "email-operation:"+key)
}

func (s *storedACSOperations) rows(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	value, err := s.connection.engine.Execute(emailStateContext(ctx), "query "+call)
	if err != nil {
		return nil, fmt.Errorf("could not read Azure send receipts")
	}
	return memql.MaterializeRows(value), nil
}

func (s *storedACSOperations) read(ctx context.Context, key string) (acsOperation, time.Time, bool, error) {
	var operation acsOperation
	rows, err := s.rows(ctx, "emailSendOperationById", map[string]any{"operationId": key})
	if err != nil || len(rows) == 0 {
		return operation, time.Time{}, false, err
	}
	row := rows[0]
	prior, ok := row["createdAt"].(time.Time)
	if !ok {
		prior, err = time.Parse(time.RFC3339Nano, fmt.Sprint(row["createdAt"]))
	}
	if err != nil {
		return operation, prior, false, fmt.Errorf("Azure send receipt revision is invalid")
	}
	sealed, _ := row["encryptedValue"].(string)
	plain, err := secret.Decrypt(sealed)
	if err != nil || json.Unmarshal([]byte(plain), &operation) != nil {
		return operation, prior, false, fmt.Errorf("could not decrypt Azure send receipt")
	}
	return operation, prior, true, nil
}

func (s *storedACSOperations) write(ctx context.Context, key string, operation acsOperation, prior time.Time) (time.Time, error) {
	data, err := json.Marshal(operation)
	if err != nil {
		return prior, err
	}
	sealed, _, err := secret.Encrypt(string(data))
	if err != nil {
		return prior, fmt.Errorf("could not encrypt Azure send receipt")
	}
	now := time.Now()
	if s.connection.now != nil {
		now = s.connection.now()
	}
	version := memql.VersionTimeAfter(prior, now)
	call, err := langparser.RenderCall("storeEmailSendOperation", map[string]any{
		"id": key, "accountId": operation.AccountID, "status": operation.Status,
		"nextPollAt": operation.NextPollAt.Format(time.RFC3339Nano), "encryptedValue": sealed, "versionTime": version.Format(time.RFC3339Nano),
	})
	if err == nil {
		_, err = s.connection.engine.Execute(emailStateContext(ctx), "mutation "+call)
	}
	if err != nil {
		return prior, fmt.Errorf("could not persist Azure send receipt")
	}
	return version, nil
}

func (s *storedACSOperations) recent(ctx context.Context, account string) ([]map[string]any, error) {
	rows, err := s.rows(ctx, "emailRecentSendOperations", map[string]any{"accountId": account})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		op, _, found, err := s.read(ctx, memql.BareShortId(fmt.Sprint(row["id"])))
		if err != nil {
			return nil, err
		}
		if found && op.AccountID == account {
			out = append(out, map[string]any{"operationId": op.ID, "status": op.Status, "detail": op.Detail, "submittedAt": op.SubmittedAt, "checkedAt": op.CheckedAt})
		}
	}
	return out, nil
}
