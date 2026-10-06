package release

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

type recordingReleaseApp struct {
	t         *testing.T
	mints     int
	lookupErr error
}

func (a *recordingReleaseApp) InstallationForRepo(_ context.Context, owner, repo string) (int64, error) {
	if owner != "acme" || repo != "widget" {
		a.t.Fatalf("installation lookup = %s/%s", owner, repo)
	}
	return 17, a.lookupErr
}

func (a *recordingReleaseApp) ScopedInstallationToken(_ context.Context, installation int64, repos []string, permissions map[string]string) (string, time.Time, error) {
	if installation != 17 || !reflect.DeepEqual(repos, []string{"widget"}) || !reflect.DeepEqual(permissions, map[string]string{"contents": "write"}) {
		a.t.Fatalf("token exceeded the release scope: %d %v %v", installation, repos, permissions)
	}
	a.mints++
	return fmt.Sprintf("ephemeral-%d", a.mints), time.Now().Add(time.Hour), nil
}

func TestReleaseAppTokenIsFreshAndLimitedToOneRepository(t *testing.T) {
	app := &recordingReleaseApp{t: t}
	r := resolver{
		env: func(name string) string {
			if name == RepoVariableName {
				return "acme/widget"
			}
			return ""
		},
		appToken: func(ctx context.Context, repo repoRef) (string, error) { return mintReleaseToken(ctx, app, repo) },
	}
	for n := 1; n <= 2; n++ {
		settings, err := r.loadSettings(context.Background())
		if err != nil || settings.token != fmt.Sprintf("ephemeral-%d", n) {
			t.Fatalf("settings for call %d: %v", n, err)
		}
	}
	app.lookupErr = errors.New("installation removed")
	if _, err := r.loadSettings(context.Background()); RefusalCode(err) != CodeCredentialUnavailable {
		t.Fatalf("revoked installation: %v", err)
	}
	if app.mints != 2 {
		t.Fatal("minted despite a refused installation lookup")
	}
}

func TestReleaseAppTokenDoesNotOverrideAnExplicitCredential(t *testing.T) {
	r := resolver{
		env: func(name string) string {
			if name == RepoVariableName {
				return "acme/widget"
			}
			if name == SecretName {
				return "explicit"
			}
			return ""
		},
		appToken: func(context.Context, repoRef) (string, error) {
			t.Fatal("app must not replace the explicit credential")
			return "", nil
		},
	}
	s, err := r.loadSettings(context.Background())
	if err != nil || s.token != "explicit" {
		t.Fatalf("explicit credential was not retained: %v", err)
	}
}

func TestReleaseAppTokenRefusalDoesNotExposeProviderData(t *testing.T) {
	r := resolver{
		env: func(name string) string {
			if name == RepoVariableName {
				return "acme/widget"
			}
			return ""
		},
		appToken: func(context.Context, repoRef) (string, error) {
			return "bearer-private", errors.New("upstream echoed private-key")
		},
	}
	_, err := r.loadSettings(context.Background())
	if RefusalCode(err) != CodeCredentialUnavailable || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe refusal: %v", err)
	}
}

func TestReleaseAppCredentialIsNeverResolvedForANonOwner(t *testing.T) {
	i := walledIntegration(t)
	i.resolver.env = func(name string) string {
		if name == RepoVariableName {
			return "acme/widget"
		}
		return ""
	}
	i.resolver.appToken = func(context.Context, repoRef) (string, error) {
		t.Fatal("non-owner reached App credentials")
		return "", nil
	}
	if _, err := i.Cut(actorContext(auth.RoleAdmin), CutRequest{Bump: "patch", DryRun: true}); err == nil {
		t.Fatal("non-owner accepted")
	}
}
