package packages

import (
	"context"
	"database/sql"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// SetGitHubGrantDB wires the direct shared database used to serialize token
// rotation, reconnect, and disconnect across all nodes.
func (i *Integration) SetGitHubGrantDB(getter func() *sql.DB) { i.githubGrantDB = getter }

func (s *store) withGrantGate(ctx context.Context, row map[string]any, fn func(context.Context) error) error {
	owner, externalID := rowString(row, "ownerUserId"), rowString(row, "externalId")
	if s.grantGate != nil {
		return s.grantGate(ctx, owner, externalID, fn)
	}
	var db *sql.DB
	if s.directDB != nil {
		db = s.directDB()
	}
	return githubconnect.WithGrantGate(ctx, db, owner, externalID, fn)
}
