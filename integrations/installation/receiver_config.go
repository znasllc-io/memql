package installation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
type receiverApplication struct {
	Key       string `json:"key"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type receiverApplicationSnapshot struct {
	key         string
	application argocd.Snapshot
	target      argocd.Target
	render      argocd.RenderSpec
}

type receiverImageBinding struct {
	Application string `json:"application,omitempty"`
	Group       string `json:"group"`
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Field       string `json:"field"`
	Container   string `json:"container"`
	Component   string `json:"component"`
	Artifact    string `json:"artifact"`
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
	Health             *receiverHealth           `json:"health,omitempty"`
	FormatVersion      int                       `json:"formatVersion"`
	InstallationID     string                    `json:"installationId"`
	Platform           string                    `json:"platform"`
	Application        receiverNamed             `json:"application,omitempty"`
	Applications       []receiverApplication     `json:"applications,omitempty"`
	SourceRepositories []argocd.SourceRepository `json:"sourceRepositories,omitempty"`
	Renderer           receiverRenderer          `json:"renderer"`
	Collector          receiverCollector         `json:"collector"`
	Catalog            receiverCatalog           `json:"catalog"`
	Rollback           receiverSelection         `json:"rollback"`
	Registries         []receiverRegistry        `json:"registries"`
	ImageBindings      []receiverImageBinding    `json:"imageBindings"`
	Dependencies       []receiverDependency      `json:"dependencies"`
	Protected          []receiverProtected       `json:"protected"`
}

// The snapshot owns private configuration and credentials. Only its digest can
// be persisted; every replacement receiver obtains and validates fresh reads.
type receiverSnapshot struct {
	healthCredential, healthRoots string
	configuration                 receiverConfiguration
	digest                        string
	invariantDigest               string
	observed                      time.Time
	applications                  []receiverApplicationSnapshot
	application                   argocd.Snapshot
	applicationTarget             argocd.Target
	render                        argocd.RenderSpec
	catalog                       Catalog
	registries                    []ociregistry.Target
	bindings                      []imageBinding
	dependencies                  []pipelines.ReleaseDependency
	protected                     []resourceIdentity
	pullCredential                string
	rendererAddress               string
	rendererTLS                   *tls.Config
}

func (s *receiverSnapshot) String() string {
	if s == nil {
		return "[uninitialized installation configuration]"
	}
	return fmt.Sprintf("installation configuration digest=%s", s.digest)
}
func (s *receiverSnapshot) GoString() string { return s.String() }

func readReceiver(ctx context.Context, api argocd.API, namespace, name string, factory CatalogFactory) (*receiverSnapshot, error) {
	return readReceiverInputs(ctx, api, namespace, name, factory, nil)
}

// A continuation substitutes a stricter owned-intent requirement for the idle
// baseline requirement. The actual Application bytes are never rewritten.
func readReceiverInputs(ctx context.Context, api argocd.API, namespace, name string, factory CatalogFactory, active *argocd.Intent) (*receiverSnapshot, error) {
	var intents map[string]argocd.Intent
	if active != nil {
		intents = map[string]argocd.Intent{"": *active}
	}
	return readReceiverGroupInputs(ctx, api, namespace, name, factory, intents)
}

func readReceiverGroupInputs(ctx context.Context, api argocd.API, namespace, name string, factory CatalogFactory, active map[string]argocd.Intent) (*receiverSnapshot, error) {
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
	if len(raw) > 256<<10 || receiverJSON([]byte(raw), &cfg) != nil || (cfg.FormatVersion != 1 && cfg.FormatVersion != 2) || !identifier.MatchString(cfg.InstallationID) ||
		(cfg.Platform != "linux/arm64" && cfg.Platform != "linux/amd64") || len(cfg.ImageBindings) == 0 || len(cfg.ImageBindings) > 512 || len(cfg.Registries) == 0 || len(cfg.Registries) > 512 || len(cfg.Dependencies) > 512 || len(cfg.Protected) == 0 || len(cfg.Protected) > 128 || cfg.Collector.CloneInstallationID < 0 ||
		!artifactDigest.MatchString(cfg.Rollback.CandidateID) || !artifactDigest.MatchString(cfg.Rollback.CatalogDigest) {
		return nil, errors.New("installation requires complete bounded receiving configuration")
	}
	if _, err := immutableImage(cfg.Collector.Image); err != nil {
		return nil, errors.New("installation collector image must be immutable")
	}
	apps, err := cfg.applicationNames()
	if err != nil {
		return nil, err
	}
	if err := argocd.ValidateSourceRepositories(cfg.SourceRepositories); err != nil {
		return nil, err
	}
	if cfg.FormatVersion == 2 {
		cfg.Application = receiverNamed{apps[0].Namespace, apps[0].Name}
	}
	s := &receiverSnapshot{configuration: cfg, observed: time.Now().UTC()}
	if cfg.Health != nil {
		var err error
		s.healthCredential, err = r.secret(ctx, namespace, cfg.Health.Credential)
		if err != nil {
			return nil, err
		}
		if cfg.Health.RootCA != nil {
			s.healthRoots, err = r.secret(ctx, namespace, *cfg.Health.RootCA)
			if err != nil {
				return nil, err
			}
		}
	}
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
		if cfg.FormatVersion == 2 {
			found := false
			for _, app := range apps {
				if app.Key == b.Application {
					found = true
				}
			}
			if !found {
				return nil, errors.New("image binding must name one configured application")
			}
		} else if b.Application != "" {
			return nil, errors.New("single application binding cannot name a group member")
		}
		seen[slot] = true
		s.bindings = append(s.bindings, imageBinding{Slot: slot, Component: b.Component, Artifact: b.Artifact, Application: b.Application})
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
	for _, app := range apps {
		member := *s
		member.configuration.Application = receiverNamed{app.Namespace, app.Name}
		var intent *argocd.Intent
		if v, ok := active[app.Key]; ok {
			copy := v
			intent = &copy
		}
		if err := member.observeArgo(ctx, r, intent); err != nil {
			return nil, err
		}
		s.applications = append(s.applications, receiverApplicationSnapshot{app.Key, member.application, member.applicationTarget, member.render})
	}
	if len(active) > len(apps) {
		return nil, errors.New("continuation has undeclared application intents")
	}
	for key := range active {
		if _, err := s.applicationByKey(key); err != nil {
			return nil, err
		}
	}
	s.application, s.applicationTarget, s.render = s.applications[0].application, s.applications[0].target, s.applications[0].render
	for _, app := range s.applications {
		if sourceRepository(app.render.Source) != sourceRepository(s.render.Source) || app.render.Namespace != s.render.Namespace || (len(active) == 0 && sourceRevision(app.render.Source) != sourceRevision(s.render.Source)) {
			return nil, errors.New("installation applications must share one repository, destination and immutable baseline")
		}
	}
	if cfg.FormatVersion == 2 {
		for i := range s.bindings {
			member, err := s.applicationByKey(s.bindings[i].Application)
			if err != nil || member.render.AppName == "" {
				return nil, errors.New("image binding has no verified application render scope")
			}
			s.bindings[i].Application = member.render.AppName
		}
	}
	if err := s.observeRenderer(ctx, r); err != nil {
		return nil, err
	}
	s.digest, err = r.finish(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]receiverNamed, len(apps))
	for i, app := range apps {
		names[i] = receiverNamed{app.Namespace, app.Name}
	}
	s.invariantDigest, err = r.configurationInvariant(names...)
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

func (c receiverConfiguration) applicationNames() ([]receiverApplication, error) {
	if c.FormatVersion == 1 {
		if len(c.Applications) != 0 || !receiverDNS.MatchString(c.Application.Namespace) || !receiverDNS.MatchString(c.Application.Name) {
			return nil, errors.New("single application configuration is invalid")
		}
		return []receiverApplication{{Namespace: c.Application.Namespace, Name: c.Application.Name}}, nil
	}
	if c.FormatVersion != 2 || c.Application != (receiverNamed{}) || len(c.Applications) == 0 || len(c.Applications) > 8 {
		return nil, errors.New("installation group requires one to eight named applications")
	}
	out := append([]receiverApplication(nil), c.Applications...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	keys, targets := map[string]bool{}, map[string]bool{}
	for _, app := range out {
		target := app.Namespace + "/" + app.Name
		if !identifier.MatchString(app.Key) || len(app.Key) > 48 || !receiverDNS.MatchString(app.Namespace) || !receiverDNS.MatchString(app.Name) || app.Namespace != out[0].Namespace || keys[app.Key] || targets[target] {
			return nil, errors.New("installation application identities are invalid or duplicated")
		}
		keys[app.Key], targets[target] = true, true
	}
	return out, nil
}
func (s *receiverSnapshot) applicationByKey(key string) (receiverApplicationSnapshot, error) {
	for _, app := range s.applications {
		if app.key == key {
			return app, nil
		}
	}
	// Existing native single-application fixtures predate the group projection.
	if len(s.applications) == 0 && key == "" && s.applicationTarget.Name != "" {
		return receiverApplicationSnapshot{"", s.application, s.applicationTarget, s.render}, nil
	}
	return receiverApplicationSnapshot{}, errors.New("application is outside the admitted installation")
}
func (s *receiverSnapshot) applicationMembers() []receiverApplicationSnapshot {
	if len(s.applications) != 0 {
		return append([]receiverApplicationSnapshot(nil), s.applications...)
	}
	return []receiverApplicationSnapshot{{"", s.application, s.applicationTarget, s.render}}
}
func sourceRepository(source json.RawMessage) string {
	var v struct {
		RepoURL string `json:"repoURL"`
	}
	_ = json.Unmarshal(source, &v)
	return v.RepoURL
}
