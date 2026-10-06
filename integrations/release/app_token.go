package release

import (
	"context"
	"time"
)

type releaseApp interface {
	InstallationForRepo(context.Context, string, string) (int64, error)
	ScopedInstallationToken(context.Context, int64, []string, map[string]string) (string, time.Time, error)
}

// mintReleaseToken never requests the installation's full reach. Each owner
// call gets a new, short-lived Contents writer for its one release repository.
// The credential stays in this call's settings, never in a row or worker step.
func mintReleaseToken(ctx context.Context, app releaseApp, repo repoRef) (string, error) {
	installation, err := app.InstallationForRepo(ctx, repo.Owner, repo.Name)
	if err != nil {
		return "", err
	}
	token, _, err := app.ScopedInstallationToken(ctx, installation, []string{repo.Name}, map[string]string{"contents": "write"})
	return token, err
}
