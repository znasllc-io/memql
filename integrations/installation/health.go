package installation

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// HealthProbe is a native authenticated SDK adapter. It returns observations
// from the receiving installation, never a caller-provided readiness boolean.
type HealthProbe func(context.Context, string, string, string, string) (HealthObservation, error)
type HealthObservation struct {
	UserID           string `json:"userId"`
	IdentityID       string `json:"identityId"`
	EngineCommit     string `json:"engineCommit"`
	MigrationsDigest string `json:"migrationsDigest"`
	Nonce            string `json:"nonce"`
}
type receiverHealth struct {
	Endpoint   string          `json:"endpoint"`
	Component  string          `json:"component"`
	Credential receiverSecret  `json:"credential"`
	RootCA     *receiverSecret `json:"rootCA,omitempty"`
}

func (v HealthObservation) validate() error {
	if !identifier.MatchString(v.UserID) || !identifier.MatchString(v.IdentityID) || !commitDigest.MatchString(v.EngineCommit) || !artifactDigest.MatchString(v.MigrationsDigest) || !identifier.MatchString(v.Nonce) {
		return errors.New("installation health observation is incomplete")
	}
	return nil
}
func (s *receiverSnapshot) healthCheck(ctx context.Context, probe HealthProbe, published pipelines.VerifiedPublishedRelease) (HealthObservation, error) {
	var out HealthObservation
	h := s.configuration.Health
	if h == nil || probe == nil || s.healthCredential == "" {
		return out, errors.New("installation requires an authenticated continuity probe")
	}
	endpoint, err := url.Parse(h.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return out, errors.New("installation health requires one authenticated HTTPS gRPC endpoint")
	}
	release, err := published.Release()
	if err != nil {
		return out, err
	}
	expected := ""
	for _, component := range release.Components {
		if component.Name == h.Component {
			expected = component.Commit
		}
	}
	if !commitDigest.MatchString(expected) {
		return out, errors.New("installation health component is absent from the signed publication")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	nonce := id.NewShortId()
	out, err = probe(ctx, strings.TrimSuffix(h.Endpoint, "/"), s.healthCredential, s.healthRoots, nonce)
	if err != nil {
		return HealthObservation{}, errors.New("installation authenticated serving or migration probe failed")
	}
	if out.validate() != nil || out.Nonce != nonce || out.EngineCommit != expected {
		return HealthObservation{}, errors.New("installation serving response differs from the signed engine revision")
	}
	return out, nil
}
func (s *preparationWorkflowScope) continuityBaseline(ctx context.Context, args map[string]any) (any, error) {
	if err := noPreparationArguments(args); err != nil {
		return nil, err
	}
	// The internal preparation harness predates public updates. A public selection
	// always requires this configuration, and native promotion checks its proof.
	if s.configuration.configuration.Health == nil {
		return nil, nil
	}
	result, err := s.configuration.healthCheck(ctx, s.host.health, s.rollback)
	if err != nil {
		return nil, err
	}
	s.healthBaseline = &result
	return nil, nil
}
