package release

import (
	"context"
	"crypto/x509"
	"errors"
	"regexp"
	"slices"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/integrations/ociregistry"
)

// Registry configuration is supplied by the operator, not a candidate or
// artifact. Destination identity includes every connection/trust value and
// credential reference; rotating secret material does not change the target.
type candidateRegistryTarget struct {
	ID                  string   `json:"id"`
	Component           string   `json:"component"`
	Artifact            string   `json:"artifact"`
	Origin              string   `json:"origin"`
	Repository          string   `json:"repository"`
	Username            string   `json:"username,omitempty"`
	PasswordSecret      string   `json:"passwordSecret,omitempty"`
	BearerSecret        string   `json:"bearerSecret,omitempty"`
	TokenEndpoint       string   `json:"tokenEndpoint,omitempty"`
	TokenService        string   `json:"tokenService,omitempty"`
	BlobDownloadOrigins []string `json:"blobDownloadOrigins,omitempty"`
	RootCAPEM           string   `json:"rootCaPem,omitempty"`
	AllowLoopbackHTTP   bool     `json:"allowLoopbackHttp,omitempty"`
}

var candidateTargetName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)

func (t candidateRegistryTarget) snapshot() (candidateRegistryTarget, ociregistry.Target, string, error) {
	if !candidateTargetName.MatchString(t.ID) || !candidateTargetName.MatchString(t.Component) || !candidateTargetName.MatchString(t.Artifact) ||
		len(t.Origin) > 2048 || len(t.TokenEndpoint) > 2048 || len(t.TokenService) > 512 || len(t.Username) > 256 || len(t.RootCAPEM) > 64<<10 || len(t.BlobDownloadOrigins) > 16 {
		return t, ociregistry.Target{}, "", errors.New("registry target requires bounded explicit configuration")
	}
	if (t.Username == "") != (t.PasswordSecret == "") || (t.BearerSecret != "" && t.Username != "") ||
		(t.PasswordSecret != "" && !candidateSecretName.MatchString(t.PasswordSecret)) || (t.BearerSecret != "" && !candidateSecretName.MatchString(t.BearerSecret)) {
		return t, ociregistry.Target{}, "", errors.New("registry target requires one explicit credential reference")
	}
	t.BlobDownloadOrigins = slices.Clone(t.BlobDownloadOrigins)
	slices.Sort(t.BlobDownloadOrigins)
	for _, origin := range t.BlobDownloadOrigins {
		if len(origin) > 2048 {
			return t, ociregistry.Target{}, "", errors.New("registry blob origin exceeds its bound")
		}
	}
	target := ociregistry.Target{Origin: t.Origin, Repository: t.Repository, TokenEndpoint: t.TokenEndpoint, TokenService: t.TokenService,
		BlobDownloadOrigins: t.BlobDownloadOrigins, AllowLoopbackHTTP: t.AllowLoopbackHTTP}
	if t.RootCAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(t.RootCAPEM)) {
			return t, target, "", errors.New("registry target contains no valid root certificates")
		}
		target.RootCAs = pool
	}
	// The protocol constructor validates exact origins, repositories, token
	// endpoints and download hosts without credentials or network effects.
	if _, err := ociregistry.NewPublisher(target); err != nil {
		return t, target, "", err
	}
	fingerprint, err := workjournal.DefinitionFingerprint("release-registry-target-v1", []workjournal.StepDecl{{
		Key: "destination", Kind: workjournal.KindDeterministic, StepType: "exec", Call: map[string]any{"target": t},
	}})
	if err != nil {
		return t, target, "", err
	}
	return t, target, "sha256:" + strings.TrimPrefix(fingerprint, "work-definition-v2:"), nil
}

type candidateTargetReader struct {
	targets map[string]candidateRegistryTarget
	files   map[string]candidateFileTarget
	secret  func(context.Context, string) (string, error)
}

// check re-resolves current operator configuration against the candidate's
// approved destination. It never follows connection values supplied by DSL.
func (r candidateTargetReader) check(ctx context.Context, destination pl.ReleaseDestination) error {
	_, _, err := r.resolve(ctx, destination)
	return err
}

func (r candidateTargetReader) resolve(ctx context.Context, d pl.ReleaseDestination) (candidateRegistryTarget, ociregistry.Target, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateRegistryTarget{}, ociregistry.Target{}, err
	}
	t, ok := r.targets[d.TargetID]
	if !ok || t.ID != d.TargetID || t.Component != d.Component || t.Artifact != d.Artifact || d.Operation != "publish" || d.DesiredStateDigest != "" || d.RollbackCandidateID != "" || r.files[d.TargetID].ID != "" {
		return t, ociregistry.Target{}, errors.New("candidate has no matching operator registry target")
	}
	t, target, digest, err := t.snapshot()
	if err != nil {
		return t, target, err
	}
	if digest != d.TargetDigest {
		return t, target, errors.New("operator target changed since candidate review")
	}
	return t, target, nil
}

// publisher may be called only after the native publication ledger has bound
// approval and effect identity. Resolving configuration here repeats the drift
// check on the publishing node; the resulting publisher owns its snapshot.
func (r candidateTargetReader) publisher(ctx context.Context, d pl.ReleaseDestination) (*ociregistry.Publisher, error) {
	t, target, err := r.resolve(ctx, d)
	if err != nil {
		return nil, err
	}
	name := t.BearerSecret
	if t.PasswordSecret != "" {
		name = t.PasswordSecret
	}
	if name != "" {
		if r.secret == nil {
			return nil, errors.New("registry credential resolver is unavailable")
		}
		value, err := r.secret(ctx, name)
		if err != nil || value == "" || len(value) > 16<<10 {
			return nil, errors.New("registry credential is unavailable")
		}
		if t.PasswordSecret != "" {
			target.Username, target.Password = t.Username, value
		} else {
			target.BearerToken = value
		}
	}
	return ociregistry.NewPublisher(target)
}
