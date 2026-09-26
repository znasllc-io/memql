package packages

import (
	"context"
	"database/sql"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/memql"
)

// Hold only the owner-scoped read/open critical section. Builds run after
// release, and another replica then observes the durable live row. A missing
// direct DB refuses before any write; there is no in-process fallback.
func (s *store) withDeploymentOpeningGate(ctx context.Context, packageID string, fn func(context.Context) error) error {
	key := "package-deployment-opening:" + memql.BareShortId(packageID)
	if s.deploymentGate != nil {
		return s.deploymentGate(ctx, key, fn)
	}
	var db *sql.DB
	if s.directDB != nil {
		db = s.directDB()
	}
	return githubconnect.WithGate(ctx, db, key, fn)
}
