package release

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

// An existing draft and its tag are operator-selected authority. Creation and
// promotion are separate approved effects, never hidden in file publication.
type candidateFileTarget struct {
	ID                string                  `json:"id"`
	Component         string                  `json:"component"`
	Artifact          string                  `json:"artifact"`
	APIOrigin         string                  `json:"apiOrigin"`
	UploadOrigin      string                  `json:"uploadOrigin"`
	DownloadOrigins   []string                `json:"downloadOrigins,omitempty"`
	Repository        string                  `json:"repository"`
	Tag               string                  `json:"tag"`
	SourceCommit      string                  `json:"sourceCommit"`
	ReleaseID         int64                   `json:"releaseId"`
	Draft             *githubrelease.Metadata `json:"draft,omitempty"`
	AssetName         string                  `json:"assetName"`
	CredentialSecret  string                  `json:"credentialSecret"`
	UseGitHubApp      bool                    `json:"useGitHubApp,omitempty"`
	RootCAPEM         string                  `json:"rootCaPem,omitempty"`
	AllowLoopbackHTTP bool                    `json:"allowLoopbackHttp,omitempty"`
}

func (t candidateFileTarget) snapshot() (candidateFileTarget, githubrelease.Target, string, error) {
	if !candidateTargetName.MatchString(t.ID) || !candidateTargetName.MatchString(t.Component) || !candidateTargetName.MatchString(t.Artifact) ||
		len(t.RootCAPEM) > 64<<10 {
		return t, githubrelease.Target{}, "", errors.New("release asset target requires bounded identities and an explicit credential reference")
	}
	if t.UseGitHubApp {
		// The installed App is authority for GitHub.com only. Operator trust
		// overrides must never send its token to another server or trust root.
		if t.CredentialSecret != "" || t.APIOrigin != "https://api.github.com" || t.UploadOrigin != "https://uploads.github.com" || t.RootCAPEM != "" || t.AllowLoopbackHTTP {
			return t, githubrelease.Target{}, "", errors.New("GitHub App release targets require official GitHub origins and no credential or trust override")
		}
	} else if !candidateSecretName.MatchString(t.CredentialSecret) {
		return t, githubrelease.Target{}, "", errors.New("release asset target requires an explicit credential reference or GitHub App selection")
	}

	t.DownloadOrigins = slices.Clone(t.DownloadOrigins)
	slices.Sort(t.DownloadOrigins)
	target := githubrelease.Target{APIOrigin: t.APIOrigin, UploadOrigin: t.UploadOrigin, DownloadOrigins: t.DownloadOrigins,
		Repository: t.Repository, Tag: t.Tag, SourceCommit: t.SourceCommit, ReleaseID: t.ReleaseID, AssetName: t.AssetName, AllowLoopbackHTTP: t.AllowLoopbackHTTP}
	if t.RootCAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(t.RootCAPEM)) {
			return t, target, "", errors.New("release asset target contains no valid root certificates")
		}
		target.RootCAs = pool
	}
	if t.Draft != nil {
		owned := *t.Draft
		t.Draft = &owned
		if err := t.Draft.Validate(); err != nil {
			return t, target, "", err
		}
	}
	if t.ReleaseID == 0 && t.Draft == nil {
		return t, target, "", errors.New("a deferred release target requires exact reviewed draft metadata")
	}
	if _, err := githubrelease.NewLifecycle(target); err != nil {
		return t, target, "", err
	}
	fingerprint, err := workjournal.DefinitionFingerprint("release-file-target-v1", []workjournal.StepDecl{{
		Key: "destination", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"target": t},
	}})
	if err != nil {
		return t, target, "", err
	}
	return t, target, "sha256:" + strings.TrimPrefix(fingerprint, "work-definition-v2:"), nil
}

func (r candidateTargetReader) resolveFile(ctx context.Context, d pl.ReleaseDestination) (candidateFileTarget, githubrelease.Target, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateFileTarget{}, githubrelease.Target{}, err
	}
	t, ok := r.files[d.TargetID]
	if !ok || t.ID != d.TargetID || t.Component != d.Component || t.Artifact != d.Artifact || d.Operation != "publish" || d.DesiredStateDigest != "" || d.RollbackCandidateID != "" || r.targets[d.TargetID].ID != "" {
		return t, githubrelease.Target{}, errors.New("candidate has no unique matching release asset target")
	}
	t, target, digest, err := t.snapshot()
	if err != nil {
		return t, target, err
	}
	if digest != d.TargetDigest {
		return t, target, errors.New("operator release asset target changed since candidate review")
	}
	return t, target, nil
}

// The artifact format and source must fit the chosen destination. Choosing the
// destination and publication order remains the installed recipe's concern.
func (r candidateTargetReader) checkArtifact(ctx context.Context, d pl.ReleaseDestination, c pl.ReleaseComponent, a pl.ReleaseArtifact) error {
	if _, ok := r.files[d.TargetID]; ok {
		_, target, err := r.resolveFile(ctx, d)
		if err != nil {
			return err
		}
		if a.Kind != "file" || a.Size >= 2<<30 || c.Repository != target.Repository || c.Commit != target.SourceCommit {
			return errors.New("release asset requires a bounded file from the draft's exact source repository and commit")
		}
		return nil
	}
	if a.Kind != "oci" {
		return errors.New("registry destination requires an OCI image artifact")
	}
	t, _, err := r.resolve(ctx, d)
	if err != nil {
		return err
	}
	if t.Tag != "" && t.Tag != c.Version && t.Tag != "v"+c.Version {
		return errors.New("registry tag must name the exact approved component version")
	}
	return nil
}

func (r candidateTargetReader) filePublisher(ctx context.Context, d pl.ReleaseDestination) (*githubrelease.Publisher, error) {
	return r.boundFilePublisher(ctx, d, 0)
}

// releaseID comes only from the native draft journal. It narrows the approved
// deferred intent; it never replaces a configured existing release identity.
func (r candidateTargetReader) boundFilePublisher(ctx context.Context, d pl.ReleaseDestination, releaseID int64) (*githubrelease.Publisher, error) {
	t, target, err := r.resolveFile(ctx, d)
	if err != nil {
		return nil, err
	}
	if releaseID != 0 {
		if target.ReleaseID != 0 && target.ReleaseID != releaseID {
			return nil, errors.New("native draft binding differs from configured release")
		}
		target.ReleaseID = releaseID
	}
	if target.ReleaseID <= 0 {
		return nil, errors.New("release upload requires a native draft binding")
	}
	token, err := r.fileToken(ctx, t)
	if err != nil {
		return nil, err
	}
	target.Token = token
	return githubrelease.NewPublisher(target)
}

func (r candidateTargetReader) fileLifecycle(ctx context.Context, d pl.ReleaseDestination) (*githubrelease.Lifecycle, error) {
	t, target, err := r.resolveFile(ctx, d)
	if err != nil {
		return nil, err
	}
	target.Token, err = r.fileToken(ctx, t)
	if err != nil {
		return nil, err
	}
	return githubrelease.NewLifecycle(target)
}

func (r candidateTargetReader) fileToken(ctx context.Context, t candidateFileTarget) (string, error) {
	var token string
	var err error
	if t.UseGitHubApp {
		repo, ok := parseRepo(t.Repository)
		if !ok || r.appToken == nil {
			return "", errors.New("release GitHub App credential resolver is unavailable")
		}
		// Existing mintReleaseToken restricts each fresh token to this one
		// repository and Contents write. No credential enters a worker step.
		token, err = r.appToken(ctx, repo)
	} else {
		if r.secret == nil {
			return "", errors.New("release credential resolver is unavailable")
		}
		token, err = r.secret(ctx, t.CredentialSecret)
	}
	if err != nil || token == "" || len(token) > 16<<10 {
		return "", errors.New("release credential is unavailable")
	}
	return token, nil
}
