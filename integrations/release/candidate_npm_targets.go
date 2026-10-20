package release

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/integrations/npmregistry"
)

type candidateNPMTarget struct {
	ID                string   `json:"id"`
	Component         string   `json:"component"`
	Artifact          string   `json:"artifact"`
	Origin            string   `json:"origin"`
	PackageName       string   `json:"packageName"`
	Version           string   `json:"version"`
	DistTag           string   `json:"distTag"`
	CredentialSecret  string   `json:"credentialSecret"`
	DownloadOrigins   []string `json:"downloadOrigins,omitempty"`
	RootCAPEM         string   `json:"rootCaPem,omitempty"`
	AllowLoopbackHTTP bool     `json:"allowLoopbackHttp,omitempty"`
}

func (t candidateNPMTarget) snapshot() (candidateNPMTarget, npmregistry.Target, string, error) {
	if !candidateTargetName.MatchString(t.ID) || !candidateTargetName.MatchString(t.Component) || !candidateTargetName.MatchString(t.Artifact) || !candidateSecretName.MatchString(t.CredentialSecret) || len(t.RootCAPEM) > 64<<10 {
		return t, npmregistry.Target{}, "", errors.New("npm target requires bounded identities and an explicit credential reference")
	}
	t.DownloadOrigins = slices.Clone(t.DownloadOrigins)
	slices.Sort(t.DownloadOrigins)
	target := npmregistry.Target{Origin: t.Origin, Name: t.PackageName, Version: t.Version, DistTag: t.DistTag, DownloadOrigins: t.DownloadOrigins, AllowLoopbackHTTP: t.AllowLoopbackHTTP}
	if t.RootCAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(t.RootCAPEM)) {
			return t, target, "", errors.New("npm target contains no valid root certificates")
		}
		target.RootCAs = pool
	}
	if _, err := npmregistry.NewPublisher(target); err != nil {
		return t, target, "", err
	}
	fingerprint, err := workjournal.DefinitionFingerprint("release-npm-target-v1", []workjournal.StepDecl{{Key: "destination", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"target": t}}})
	if err != nil {
		return t, target, "", err
	}
	return t, target, "sha256:" + strings.TrimPrefix(fingerprint, "work-definition-v2:"), nil
}

func (r candidateTargetReader) resolveNPM(ctx context.Context, d pl.ReleaseDestination) (candidateNPMTarget, npmregistry.Target, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateNPMTarget{}, npmregistry.Target{}, err
	}
	t, ok := r.npm[d.TargetID]
	if !ok || t.ID != d.TargetID || t.Component != d.Component || t.Artifact != d.Artifact || d.Operation != "publish" || d.DesiredStateDigest != "" || d.RollbackCandidateID != "" || r.targets[d.TargetID].ID != "" || r.files[d.TargetID].ID != "" {
		return t, npmregistry.Target{}, errors.New("candidate has no unique matching npm target")
	}
	t, target, digest, err := t.snapshot()
	if err != nil {
		return t, target, err
	}
	if digest != d.TargetDigest {
		return t, target, errors.New("npm target changed since candidate review")
	}
	return t, target, nil
}

func (r candidateTargetReader) npmPublisher(ctx context.Context, d pl.ReleaseDestination) (*npmregistry.Publisher, error) {
	t, target, err := r.resolveNPM(ctx, d)
	if err != nil {
		return nil, err
	}
	if r.secret == nil {
		return nil, errors.New("npm credential resolver is unavailable")
	}
	target.Token, err = r.secret(ctx, t.CredentialSecret)
	if err != nil || target.Token == "" {
		return nil, errors.New("npm credential is unavailable")
	}
	return npmregistry.NewPublisher(target)
}

func npmVersionKey(t candidateNPMTarget) string {
	return registryTagKey(candidateRegistryTarget{Origin: t.Origin, Repository: "npm/" + t.PackageName, Tag: t.Version})
}
