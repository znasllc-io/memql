package app

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/pat"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/releasecatalog"
	"github.com/znasllc-io/memql/core/releasewire"
	"github.com/znasllc-io/memql/integrations/release"
	"github.com/znasllc-io/memql/sdk/go/client"
)

type installationPATVerifier interface {
	VerifyToken(context.Context, string) (*pat.Claims, error)
}

func installationReadAuthenticator(verifier installationPATVerifier) func(context.Context, string) (context.Context, error) {
	return func(ctx context.Context, token string) (context.Context, error) {
		if verifier == nil || !pat.IsPATToken(token) {
			return nil, errors.New("installation PAT verifier unavailable")
		}
		claims, err := verifier.VerifyToken(memql.ContextWithFreshRead(ctx), token)
		if err != nil || claims == nil || claims.Source != pat.SourcePAT || claims.UserId == "" || claims.PATID == "" || auth.IsRoleTestUser(claims.UserId) {
			return nil, errors.New("installation PAT was refused")
		}
		// The verifier freshly read the active PAT and its active owning user.
		// Replace all three actor surfaces; never inherit Identity's ambient
		// bootstrap/system actor or accept an owner/role from the request body.
		values := map[string]any{"sub": claims.UserId, "email": claims.Email, "name": claims.Name, "role": claims.Role}
		ctx = auth.ContextWithClaims(ctx, values)
		ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(values))
		access := auth.FallbackFromClaims(values)
		access.IdentityId = claims.PATID
		return auth.ContextWithAccess(ctx, access), nil
	}
}

// Identity is the only PAT verification host. It needs the migration checker
// for this read surface, but no publisher, installer, object-store write port
// or Kubernetes grant. Published catalogs already read the shared signed ledger.
func (a *App) mountIdentityInstallationReads(verifier installationPATVerifier) {
	if a.engine == nil {
		a.fatal("Identity installation reads require an engine")
		return
	}
	provider, ok := a.engine.IntegrationByName("release").(*release.Integration)
	if !ok {
		a.fatal("Identity installation reads require the release integration")
		return
	}
	if err := provider.ConfigureInstallationHealth(a.installationMigrationHealth); err != nil {
		a.fatal("Identity installation health could not be configured", "error", err)
		return
	}
	a.handleRoute("POST "+releasewire.ReadPath, releasecatalog.NewIdentityReadHandler(installationReadAuthenticator(verifier), func(ctx context.Context, query string) ([]client.Row, error) {
		result, err := a.engine.Execute(ctx, query)
		if err != nil {
			return nil, err
		}
		return installationReadRows(result)
	}))
}

func installationReadRows(result *memql.ExecuteResult) ([]client.Row, error) {
	if result == nil {
		return nil, errors.New("missing installation evidence")
	}
	var rows []client.Row
	switch value := result.OutputPayload().(type) {
	case map[string]any:
		return []client.Row{value}, nil
	case []map[string]any:
		for _, row := range value {
			rows = append(rows, row)
		}
		return rows, nil
	case []any:
		for _, item := range value {
			row, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("invalid installation evidence")
			}
			rows = append(rows, row)
		}
		return rows, nil
	}
	if result.Bundle == nil {
		return nil, errors.New("missing installation evidence")
	}
	for _, node := range result.Bundle.Nodes {
		if node == nil || node.Payload == nil {
			return nil, errors.New("invalid installation evidence")
		}
		row := node.Payload.AsMap()
		if _, exists := row["id"]; !exists && node.Id != "" {
			row["id"] = node.Id
		}
		rows = append(rows, row)
	}
	return rows, nil
}
