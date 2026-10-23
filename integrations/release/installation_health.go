package release

import (
	"context"
	"errors"
	"regexp"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/buildinfo"
)

type InstallationHealthCheck func(context.Context) (string, error)
type installationHealthBinding struct{ check InstallationHealthCheck }

var healthNonce = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func (i *Integration) ConfigureInstallationHealth(check InstallationHealthCheck) error {
	if i == nil || check == nil {
		return errors.New("installation health requires the native migration verifier")
	}
	if !i.installationHealth.CompareAndSwap(nil, &installationHealthBinding{check}) {
		return errors.New("installation health verifier already configured")
	}
	return nil
}
func (i *Integration) handleInstallationHealth(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := catalogReader(ctx); err != nil {
		return nil, err
	}
	nonce := asString(args["nonce"])
	if !healthNonce.MatchString(nonce) {
		return nil, errors.New("health probe requires a fresh bounded nonce")
	}
	binding := i.installationHealth.Load()
	if binding == nil {
		return nil, errors.New("installation migration verifier unavailable")
	}
	digest, err := binding.check(ctx)
	if err != nil {
		return nil, errors.New("installation database migrations are not complete")
	}
	if !stringsHasDigest(digest) || !installationCommit.MatchString(buildinfo.Commit()) {
		return nil, errors.New("installation build or migration evidence is unavailable")
	}
	actor, _ := auth.AccessFromContext(ctx)
	return resultNode("installationHealth", nonce, map[string]any{"nonce": nonce, "userId": memql.BareShortId(actor.UserId), "identityId": memql.BareShortId(actor.IdentityId), "engineCommit": buildinfo.Commit(), "migrationsDigest": digest})
}
func stringsHasDigest(value string) bool {
	return len(value) == 71 && value[:7] == "sha256:" && candidateArtifactDigest(value[7:])
}
