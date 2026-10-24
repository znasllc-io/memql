package app

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/uptrace/bun/migrate"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/installation"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// Read the serving process's registered migrations against the live ledger.
// A successful startup alone cannot prove a reconnect or deferred migration.
func (a *App) installationMigrationHealth(ctx context.Context) (string, error) {
	if a == nil || a.db == nil || a.db.MigrationError() != nil {
		return "", errors.New("installation migrations are unavailable")
	}
	migrator, err := a.db.Migrator()
	if err != nil {
		return "", err
	}
	rows, err := migrator.MigrationsWithStatus(ctx)
	if err != nil {
		return "", err
	}
	return installedMigrationDigest(rows)
}

func installedMigrationDigest(rows migrate.MigrationSlice) (string, error) {
	if len(rows) == 0 {
		return "", errors.New("installation has no registered migration evidence")
	}
	names := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Name == "" || !row.IsApplied() || seen[row.Name] {
			return "", errors.New("installation migration ledger is incomplete")
		}
		seen[row.Name] = true
		names = append(names, row.Name)
	}
	sort.Strings(names)
	body, err := json.Marshal(names)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// This adapter opens the same authenticated TLS gRPC surface that clients use.
// The protected pre-update credential must still authenticate and resolve its
// own stored user after the selected engine and workload images are running.
func installationServingProbe(ctx context.Context, endpoint, token, roots, nonce string) (installation.HealthObservation, error) {
	var out installation.HealthObservation
	if token == "" || len(token) > 16<<10 || strings.ContainsAny(token, "\r\n\t ") {
		return out, errors.New("installation continuity credential is unavailable")
	}
	var trust *x509.CertPool
	if roots != "" {
		trust = x509.NewCertPool()
		if !trust.AppendCertsFromPEM([]byte(roots)) {
			return out, errors.New("installation continuity certificate roots are invalid")
		}
	}
	conn, err := client.Connect(ctx, client.ConnectConfig{Endpoint: endpoint, Token: token, RootCAs: trust})
	if err != nil {
		return out, errors.New("installation authenticated connection could not be established")
	}
	defer conn.Close()
	query := client.NewQueryClient(conn.Dispatcher())
	health, err := query.ReleaseInstallationHealth(ctx, client.ReleaseInstallationHealthArgs{Nonce: nonce})
	if err != nil || len(health.Rows()) != 1 {
		return out, errors.New("installation serving migration evidence is unavailable")
	}
	user, err := query.CurrentUser(ctx, client.CurrentUserArgs{})
	if err != nil || len(user.Rows()) != 1 {
		return out, errors.New("installation original user record is unavailable")
	}
	return installationHealthReply(health.Rows()[0], user.Rows()[0], nonce)
}

func installationHealthReply(health, user client.Row, nonce string) (installation.HealthObservation, error) {
	var out installation.HealthObservation
	body, err := json.Marshal(health)
	if err != nil || json.Unmarshal(body, &out) != nil {
		return out, errors.New("installation serving evidence is malformed")
	}
	id, ok := user["id"].(string)
	if !ok || out.UserID == "" || out.IdentityID == "" || out.Nonce != nonce || memql.BareShortId(id) != out.UserID {
		return installation.HealthObservation{}, errors.New("installation authenticated user differs from its serving evidence")
	}
	return out, nil
}
