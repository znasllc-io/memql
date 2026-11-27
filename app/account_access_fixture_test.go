package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/memql"
)

// Transport and source-policy fixtures start with an account whose app ceiling
// has already been assigned. This is fixture setup, not a production writer;
// the transactional assignment and its refusals have engine DB coverage.
func seedAccountApps(t *testing.T, engine *memql.MemQLEngine, db *bun.DB, account string, apps ...string) {
	t.Helper()
	encoded, err := json.Marshal(apps)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload=jsonb_set(payload, '{allowedApps}', ?::jsonb) WHERE concept='v1:accounts:account' AND id=?`, string(encoded), "v1:accounts:account:"+memql.BareShortId(account))
	require.NoError(t, err)
	engine.InvalidateCacheForConcept("v1:accounts:account")
}
