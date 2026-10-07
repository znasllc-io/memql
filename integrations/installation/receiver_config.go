package installation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/ociregistry"
)

var receiverDNS = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
var receiverKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,253}$`)

// Catalog is the narrow verified-publication port implemented by the canonical
// SDK adapter in the root module. A native factory freezes these credentials;
// no DSL caller chooses an endpoint, trust root or a preverified envelope.
type Catalog interface {
	Get(context.Context, string, string, string) (pipelines.VerifiedPublishedRelease, error)
}
type CatalogFactory func(context.Context, []byte, string) (Catalog, error)

type receiverSecret struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type receiverNamed struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type receiverSelection struct {
	CandidateID   string `json:"candidateId"`
	CatalogDigest string `json:"catalogDigest"`
}
type receiverCatalog struct {
	ID         string            `json:"id"`
	Publisher  string            `json:"publisher"`
	Endpoint   string            `json:"endpoint"`
	PublicKeys map[string]string `json:"publicKeys"`
	Credential receiverSecret    `json:"credential"`
	RootCA     *receiverSecret   `json:"rootCA,omitempty"`
}
type receiverRegistry struct {
	Origin              string          `json:"origin"`
	Repository          string          `json:"repository"`
	Username            string          `json:"username,omitempty"`
	Password            *receiverSecret `json:"password,omitempty"`
	BearerToken         *receiverSecret `json:"bearerToken,omitempty"`
	RootCA              *receiverSecret `json:"rootCA,omitempty"`
	TokenEndpoint       string          `json:"tokenEndpoint,omitempty"`
	TokenService        string          `json:"tokenService,omitempty"`
	BlobDownloadOrigins []string        `json:"blobDownloadOrigins,omitempty"`
}
type receiverRenderer struct {
	Deployment string `json:"deployment"`
	Service    string `json:"service"`
	Container  string `json:"container"`
	Image      string `json:"image"`
	Profile    string `json:"profile"`
	ConfigMap  string `json:"configMap"`
	TLSSecret  string `json:"tlsSecret"`
}
type receiverCollector struct {
	Image               string          `json:"image"`
	CloneInstallationID int64           `json:"cloneInstallationId"`
	PullCredential      *receiverSecret `json:"pullCredential,omitempty"`
}
type receiverImageBinding struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Field     string `json:"field"`
	Container string `json:"container"`
	Component string `json:"component"`
	Artifact  string `json:"artifact"`
}
type receiverProtected struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
}
type receiverDependency struct {
	Component string `json:"component"`
	Requires  string `json:"requires"`
}
type receiverConfiguration struct {
	FormatVersion  int                    `json:"formatVersion"`
	InstallationID string                 `json:"installationId"`
	Platform       string                 `json:"platform"`
	Application    receiverNamed          `json:"application"`
	Renderer       receiverRenderer       `json:"renderer"`
	Collector      receiverCollector      `json:"collector"`
	Catalog        receiverCatalog        `json:"catalog"`
	Rollback       receiverSelection      `json:"rollback"`
	Registries     []receiverRegistry     `json:"registries"`
	ImageBindings  []receiverImageBinding `json:"imageBindings"`
	Dependencies   []receiverDependency   `json:"dependencies"`
	Protected      []receiverProtected    `json:"protected"`
}

// The snapshot owns private configuration and credentials. Only its digest can
// be persisted; every replacement receiver obtains and validates fresh reads.
type receiverSnapshot struct {
	configuration   receiverConfiguration
	digest          string
	observed        time.Time
	application     argocd.Snapshot
	render          argocd.RenderSpec
	catalog         Catalog
	registries      []ociregistry.Target
	bindings        []imageBinding
	dependencies    []pipelines.ReleaseDependency
	protected       []resourceIdentity
	pullCredential  string
	rendererAddress string
	rendererTLS     *tls.Config
}

func (s *receiverSnapshot) String() string {
	if s == nil {
		return "[uninitialized installation configuration]"
	}
	return fmt.Sprintf("installation configuration digest=%s", s.digest)
}
func (s *receiverSnapshot) GoString() string { return s.String() }

func readReceiver(ctx context.Context, api argocd.API, namespace, name string, factory CatalogFactory) (*receiverSnapshot, error) {
	if _, err := preparationActor(ctx); err != nil {
		return nil, err
	}
	if api == nil || factory == nil {
		return nil, errors.New("installation receiving adapters are unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	r := newReceiverReads(api)
	cm, err := r.object(ctx, "v1", "ConfigMap", "configmaps", namespace, name)
	if err != nil {
		return nil, err
	}
	raw := resourceText(resourceMap(cm, "data"), "installation.json")
	var cfg receiverConfiguration
	if len(raw) > 256<<10 || receiverJSON([]byte(raw), &cfg) != nil || cfg.FormatVersion != 1 || !identifier.MatchString(cfg.InstallationID) ||
		(cfg.Platform != "linux/arm64" && cfg.Platform != "linux/amd64") || len(cfg.ImageBindings) == 0 || len(cfg.ImageBindings) > 512 || len(cfg.Registries) == 0 || len(cfg.Registries) > 512 || len(cfg.Dependencies) > 512 || len(cfg.Protected) == 0 || len(cfg.Protected) > 128 || cfg.Collector.CloneInstallationID < 0 ||
		!artifactDigest.MatchString(cfg.Rollback.CandidateID) || !artifactDigest.MatchString(cfg.Rollback.CatalogDigest) {
		return nil, errors.New("installation requires complete bounded receiving configuration")
	}
	if _, err := immutableImage(cfg.Collector.Image); err != nil {
		return nil, errors.New("installation collector image must be immutable")
	}
	s := &receiverSnapshot{configuration: cfg, observed: time.Now().UTC()}
	token, err := r.secret(ctx, namespace, cfg.Catalog.Credential)
	if err != nil {
		return nil, err
	}
	roots := ""
	if cfg.Catalog.RootCA != nil {
		roots, err = r.secret(ctx, namespace, *cfg.Catalog.RootCA)
		if err != nil {
			return nil, err
		}
	}
	catalogBody, _ := json.Marshal(map[string]any{"formatVersion": 1, "sources": []any{map[string]any{"id": cfg.Catalog.ID, "publisher": cfg.Catalog.Publisher, "endpoint": cfg.Catalog.Endpoint, "publicKeys": cfg.Catalog.PublicKeys, "credentialSecret": "INSTALLATION_CATALOG_TOKEN", "rootCaPem": roots}}})
	s.catalog, err = factory(ctx, catalogBody, token)
	if err != nil || s.catalog == nil {
		return nil, errors.New("installation publisher configuration is invalid")
	}
	for _, registry := range cfg.Registries {
		target := ociregistry.Target{Origin: registry.Origin, Repository: registry.Repository, Username: registry.Username, TokenEndpoint: registry.TokenEndpoint, TokenService: registry.TokenService, BlobDownloadOrigins: append([]string(nil), registry.BlobDownloadOrigins...)}
		if registry.Password != nil {
			target.Password, err = r.secret(ctx, namespace, *registry.Password)
			if err != nil {
				return nil, err
			}
		}
		if registry.BearerToken != nil {
			target.BearerToken, err = r.secret(ctx, namespace, *registry.BearerToken)
			if err != nil {
				return nil, err
			}
		}
		if registry.RootCA != nil {
			pem, err := r.secret(ctx, namespace, *registry.RootCA)
			if err != nil {
				return nil, err
			}
			target.RootCAs = x509.NewCertPool()
			if !target.RootCAs.AppendCertsFromPEM([]byte(pem)) {
				return nil, errors.New("installation registry trust is invalid")
			}
		}
		if _, err := ociregistry.NewAvailabilityVerifier(target); err != nil {
			return nil, errors.New("installation registry configuration is invalid")
		}
		s.registries = append(s.registries, target)
	}
	if cfg.Collector.PullCredential != nil {
		s.pullCredential, err = r.secret(ctx, namespace, *cfg.Collector.PullCredential)
		if err != nil {
			return nil, err
		}
	}
	seen := map[imageSlot]bool{}
	for _, b := range cfg.ImageBindings {
		slot := imageSlot{Resource: resourceIdentity{Group: b.Group, Kind: b.Kind, Namespace: b.Namespace, Name: b.Name}, Field: b.Field, Container: b.Container}
		if !kubernetesSegment.MatchString(b.Kind) || !kubernetesSegment.MatchString(b.Name) || !receiverDNS.MatchString(b.Namespace) || !receiverKey.MatchString(b.Field) || (!receiverKey.MatchString(b.Container) && !(b.Field == "imageName" && b.Container == "")) || !receiverKey.MatchString(b.Component) || !receiverKey.MatchString(b.Artifact) || seen[slot] {
			return nil, errors.New("installation image binding is invalid or duplicated")
		}
		seen[slot] = true
		s.bindings = append(s.bindings, imageBinding{Slot: slot, Component: b.Component, Artifact: b.Artifact})
	}
	for _, d := range cfg.Dependencies {
		s.dependencies = append(s.dependencies, pipelines.ReleaseDependency{Component: d.Component, Requires: d.Requires})
	}
	for _, p := range cfg.Protected {
		key := resourceIdentity{Kind: p.Kind, Namespace: p.Namespace, Name: p.Name}
		if _, err := sensitivePath(key); err != nil {
			return nil, err
		}
		s.protected = append(s.protected, key)
	}
	if err := s.observeArgo(ctx, r); err != nil {
		return nil, err
	}
	if err := s.observeRenderer(ctx, r); err != nil {
		return nil, err
	}
	s.digest, err = r.finish(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Every failure is content-free, including upstream configuration/Secret errors.
func receiverStringList(value any) ([]string, error) {
	raw, ok := value.([]any)
	if !ok || len(raw) > 4096 {
		return nil, errors.New("installation resource list is invalid")
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		if !ok || len(s) > 2048 || strings.ContainsAny(s, "\x00\r\n") {
			return nil, errors.New("installation resource string is invalid")
		}
		out[i] = s
	}
	return out, nil
}
