package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
)

// One encrypted store for setup sessions and organization transports. A caller
// supplies a logical key only after its actor/organization check. Reads bypass
// process caches and updates serialize in PostgreSQL across every replica.
type connectionStore struct {
	engine   ConfigWriter
	database func() *sql.DB
	now      func() time.Time
}

func emailStateContext(ctx context.Context) context.Context {
	ctx = auth.ContextWithAccess(ctx, auth.SystemActor("email-connection"))
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "email-connection"})
	return memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(ctx))
}

func emailStateID(key string) string { return string(id.New().FromString("email-state:" + key)) }

func (s *connectionStore) read(ctx context.Context, key string, target any) (bool, time.Time, error) {
	if s == nil || s.engine == nil {
		return false, time.Time{}, fmt.Errorf("email connection storage is unavailable")
	}
	q := strings.Replace(renderConfigCall("emailConnectionStateById", map[string]string{"stateId": emailStateID(key)}), "mutation ", "query ", 1)
	result, err := s.engine.Execute(emailStateContext(ctx), q)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("could not read email connection state")
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		return false, time.Time{}, nil
	}
	if len(rows) != 1 {
		return false, time.Time{}, fmt.Errorf("email connection state is ambiguous")
	}
	row := rows[0]
	revision, ok := row["createdAt"].(time.Time)
	if !ok {
		revision, err = time.Parse(time.RFC3339Nano, fmt.Sprint(row["createdAt"]))
		if err != nil {
			return false, time.Time{}, fmt.Errorf("email connection revision is invalid")
		}
	}
	sealed, _ := row["encryptedValue"].(string)
	plain, err := secret.Decrypt(sealed)
	if err != nil || json.Unmarshal([]byte(plain), target) != nil {
		return false, time.Time{}, fmt.Errorf("could not decrypt email connection state")
	}
	return true, revision, nil
}

// change executes the supplied update while holding a database transaction
// lock. A network-backed update must itself use bounded request deadlines.
func (s *connectionStore) change(ctx context.Context, key string, target any, change func(found bool) error) error {
	var db *sql.DB
	if s != nil && s.database != nil {
		db = s.database()
	}
	release, err := memql.AcquireWriteGate(ctx, db, "email-connection:"+emailStateID(key))
	if err != nil {
		return fmt.Errorf("email connection coordination is unavailable")
	}
	defer release()
	found, prior, err := s.read(ctx, key, target)
	if err != nil {
		return err
	}
	if err = change(found); err != nil {
		return err
	}
	data, err := json.Marshal(target)
	if err != nil || len(data) > 128<<10 {
		return fmt.Errorf("email connection state exceeds its storage limit")
	}
	sealed, _, err := secret.Encrypt(string(data))
	if err != nil {
		return fmt.Errorf("could not encrypt email connection state")
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	_, err = s.engine.Execute(emailStateContext(ctx), renderConfigCall("storeEmailConnectionState", map[string]string{"id": emailStateID(key), "encryptedValue": sealed, "versionTime": memql.VersionTimeAfter(prior, now).Format(time.RFC3339Nano)}))
	if err != nil {
		return fmt.Errorf("could not persist email connection state")
	}
	return nil
}
