package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestFileAppCredentialsAreFreshScopedAndWiredThroughConfiguration(t *testing.T) {
	target := fileTargetFixture()
	_, _, previous, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	target.UseGitHubApp, target.CredentialSecret = true, ""
	_, _, digest, err := target.snapshot()
	if err != nil || previous == digest {
		t.Fatal("credential selection was not bound into target identity", err)
	}
	calls := 0
	cfg := candidateOperatorConfiguration{FormatVersion: 1, Sources: []candidateVersionSource{{Component: "engine", Repository: target.Repository, Path: "VERSION"}}, ReleaseAssets: []candidateFileTarget{target}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	i := NewIntegration(nil, &tripwireEngine{t: t}, resolver{
		systemVariable: func(context.Context, string) (string, error) { return string(raw), nil },
		systemSecret: func(context.Context, string) (string, error) {
			t.Fatal("App publication reached stored credentials")
			return "", nil
		},
		appToken: func(_ context.Context, repo repoRef) (string, error) {
			if repo.String() != target.Repository {
				t.Fatal("token scope differs from the approved repository", repo)
			}
			calls++
			return "ephemeral-test-token", nil
		},
	})
	if err := i.ConfigureCandidates(CandidateDependencies{Database: func() *sql.DB { t.Fatal("configuration touched SQL"); return nil }, Library: &candidateLibraryFixture{t: t}}); err != nil {
		t.Fatal(err)
	}
	p, err := i.configuredCandidate(ownerCtx())
	if err != nil || calls != 0 {
		t.Fatal("configuration resolved publication credentials", err)
	}
	d := pl.ReleaseDestination{TargetID: target.ID, TargetDigest: digest, Component: target.Component, Artifact: target.Artifact, Operation: "publish"}
	for range 2 {
		if _, err := p.targetReader.fileLifecycle(ownerCtx(), d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.targetReader.filePublisher(ownerCtx(), d); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("each native operation must mint fresh authority; got %d resolutions", calls)
	}
	if _, err := p.targetReader.filePublisher(context.Background(), d); err == nil || calls != 3 {
		t.Fatal("unowned publication reached App authority")
	}
}

func TestFileAppAuthorityRefusesOriginTrustAndCredentialDrift(t *testing.T) {
	target := fileTargetFixture()
	target.UseGitHubApp, target.CredentialSecret = true, ""
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d := pl.ReleaseDestination{TargetID: target.ID, TargetDigest: digest, Component: target.Component, Artifact: target.Artifact, Operation: "publish"}
	for name, change := range map[string]func(*candidateFileTarget){
		"api origin":       func(x *candidateFileTarget) { x.APIOrigin = "https://example.com" },
		"upload origin":    func(x *candidateFileTarget) { x.UploadOrigin = "https://example.com" },
		"secret override":  func(x *candidateFileTarget) { x.CredentialSecret = "OTHER_TOKEN" },
		"root override":    func(x *candidateFileTarget) { x.RootCAPEM = "custom trust" },
		"loopback":         func(x *candidateFileTarget) { x.AllowLoopbackHTTP = true },
		"repository drift": func(x *candidateFileTarget) { x.Repository = "acme/another" },
		"selection drift":  func(x *candidateFileTarget) { x.UseGitHubApp, x.CredentialSecret = false, "OTHER_TOKEN" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := target
			change(&changed)
			r := candidateTargetReader{files: map[string]candidateFileTarget{target.ID: changed}, appToken: func(context.Context, repoRef) (string, error) {
				t.Fatal("changed authority reached App credentials")
				return "", nil
			}, secret: func(context.Context, string) (string, error) {
				t.Fatal("changed authority reached a stored credential")
				return "", nil
			}}
			if _, err := r.filePublisher(ownerCtx(), d); err == nil {
				t.Fatal("changed authority admitted publication")
			}
			if _, err := r.fileLifecycle(ownerCtx(), d); err == nil {
				t.Fatal("changed authority admitted lifecycle operations")
			}
		})
	}
}

func TestFileAppCredentialFailureNeverFallsBackOrExposesErrors(t *testing.T) {
	target := fileTargetFixture()
	target.UseGitHubApp, target.CredentialSecret = true, ""
	_, _, digest, err := target.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d := pl.ReleaseDestination{TargetID: target.ID, TargetDigest: digest, Component: target.Component, Artifact: target.Artifact, Operation: "publish"}
	for _, mint := range []func(context.Context, repoRef) (string, error){nil,
		func(context.Context, repoRef) (string, error) { return "", errors.New("private credential detail") },
		func(context.Context, repoRef) (string, error) { return "", nil },
		func(context.Context, repoRef) (string, error) { return strings.Repeat("x", 17<<10), nil },
	} {
		r := candidateTargetReader{files: map[string]candidateFileTarget{target.ID: target}, appToken: mint,
			secret: func(context.Context, string) (string, error) {
				t.Fatal("App failure fell back to a stored credential")
				return "", nil
			}}
		_, err := r.filePublisher(ownerCtx(), d)
		if err == nil || strings.Contains(err.Error(), "private credential detail") {
			t.Fatal("missing authority was accepted or disclosed", err)
		}
	}
}
