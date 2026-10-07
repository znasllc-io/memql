package release

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

const CandidateConfigurationVariable = "MEMQL_RELEASE_CANDIDATES"

// CandidateLibrary is the native retained-artifact authority, not editable
// Library metadata. App wiring supplies the same store used by pipeline jobs.
type CandidateLibrary interface {
	pipelinesteps.LibraryArtifactLifecycle
	pipelinesteps.LibraryReceiptOpener
}

// CandidateDependencies carries native construction dependencies only. Neither
// DSL arguments nor operator JSON can supply a database, reader or HTTP client.
type CandidateDependencies struct {
	Database     func() *sql.DB
	Library      CandidateLibrary
	SourceClient *Client
}

// ConfigureCandidates binds once, before serving requests. Atomic publication
// prevents a concurrent request from observing partially installed authority.
func (i *Integration) ConfigureCandidates(deps CandidateDependencies) error {
	if i == nil || deps.Database == nil || deps.Library == nil {
		return errors.New("release candidates require a database and retained-artifact store")
	}
	if !i.candidateDeps.CompareAndSwap(nil, &deps) {
		return errors.New("release candidate dependencies are already configured")
	}
	return nil
}

type candidateOperatorConfiguration struct {
	FormatVersion int                       `json:"formatVersion"`
	Sources       []candidateVersionSource  `json:"sources"`
	Registries    []candidateRegistryTarget `json:"registries"`
	ReleaseAssets []candidateFileTarget     `json:"releaseAssets,omitempty"`
}

func decodeCandidateObject(body []byte, into any) error {
	if len(body) == 0 || len(body) > 1<<20 {
		return errors.New("release candidate input is empty or exceeds one MiB")
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("release candidate input must be an object")
	}
	keys := json.NewDecoder(bytes.NewReader(body))
	keys.UseNumber()
	if err := candidateUniqueJSON(keys, 0); err != nil {
		return errors.New("release candidate input contains ambiguous or excessive JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.New("release candidate input contains invalid or unknown fields")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("release candidate input must contain exactly one object")
	}
	return nil
}

// encoding/json matches struct fields without regard to case. Refuse duplicate
// spellings too, so the owner cannot review one value and execute another.
func candidateUniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("excessive JSON nesting")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[strings.ToLower(name)] {
				return errors.New("duplicate JSON field")
			}
			seen[strings.ToLower(name)] = true
			if err := candidateUniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := candidateUniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func (i *Integration) candidateStorage(ctx context.Context) (*candidatePreparer, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return nil, err
	}
	if i == nil {
		return nil, errors.New("release candidates are unavailable")
	}
	deps := i.candidateDeps.Load()
	if deps == nil {
		if i.candidateDB == nil {
			return nil, errors.New("release candidate storage is not configured on this node")
		}
		return &candidatePreparer{ledger: &candidateLedger{db: i.candidateDB}, logger: i.logger}, nil
	}
	return &candidatePreparer{ledger: &candidateLedger{db: deps.Database}, library: deps.Library, logger: i.logger}, nil
}

func (i *Integration) configuredCandidate(ctx context.Context) (*candidatePreparer, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	if p.library == nil {
		return nil, errors.New("release candidate artifact storage is unavailable on this node")
	}
	if i.resolver.systemVariable == nil || i.store == nil || i.store.engine == nil {
		return nil, errors.New("release candidate configuration is unavailable")
	}
	raw, err := i.resolver.systemVariable(memql.ContextWithFreshRead(ctx), CandidateConfigurationVariable)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, errors.New("release candidates require explicit operator configuration")
	}
	var cfg candidateOperatorConfiguration
	if err := decodeCandidateObject([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	if cfg.FormatVersion != 1 || len(cfg.Sources) == 0 || len(cfg.Sources) > 64 || len(cfg.Registries)+len(cfg.ReleaseAssets) == 0 || len(cfg.Registries)+len(cfg.ReleaseAssets) > 1024 {
		return nil, errors.New("release candidate configuration requires bounded version sources and publication targets")
	}
	client := i.candidateDeps.Load().SourceClient
	if client == nil {
		client = i.github
	}
	versions := &candidateVersionReader{client: client, resolver: i.resolver, sources: map[string]candidateVersionSource{}}
	for _, source := range cfg.Sources {
		if err := source.validate(); err != nil {
			return nil, err
		}
		if !candidateTargetName.MatchString(source.Component) {
			return nil, errors.New("invalid release component identity")
		}
		if versions.sources[source.Component].Component != "" {
			return nil, errors.New("duplicate release version source")
		}
		versions.sources[source.Component] = source
	}
	targets := &candidateTargetReader{targets: map[string]candidateRegistryTarget{}, files: map[string]candidateFileTarget{}, secret: i.resolver.systemSecret}
	for _, target := range cfg.Registries {
		owned, _, _, err := target.snapshot()
		if err != nil {
			return nil, err
		}
		if targets.targets[target.ID].ID != "" || versions.sources[target.Component].Component == "" {
			return nil, errors.New("release registry has a duplicate identity or no version source")
		}
		targets.targets[target.ID] = owned
	}
	for _, target := range cfg.ReleaseAssets {
		owned, _, _, err := target.snapshot()
		if err != nil {
			return nil, err
		}
		if targets.targets[target.ID].ID != "" || targets.files[target.ID].ID != "" || versions.sources[target.Component].Repository != target.Repository {
			return nil, errors.New("release asset target has a duplicate identity or no matching source repository")
		}
		targets.files[target.ID] = owned
	}
	p.evidence, p.versionReader, p.targetReader = candidateEvidenceReader{engine: i.store.engine}, versions, targets
	return p, nil
}

func (i *Integration) handleCandidateConfiguration(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.configuredCandidate(ctx)
	if err != nil {
		return nil, err
	}
	sources := make([]candidateVersionSource, 0, len(p.versionReader.sources))
	for _, source := range p.versionReader.sources {
		sources = append(sources, source)
	}
	slices.SortFunc(sources, func(a, b candidateVersionSource) int { return strings.Compare(a.Component, b.Component) })
	type targetView struct {
		pl.ReleaseDestination
		Origin               string `json:"origin"`
		Repository           string `json:"repository"`
		Kind                 string `json:"kind"`
		Tag                  string `json:"tag,omitempty"`
		SourceCommit         string `json:"sourceCommit,omitempty"`
		ReleaseID            int64  `json:"releaseId,omitempty"`
		AssetName            string `json:"assetName,omitempty"`
		CredentialConfigured bool   `json:"credentialConfigured"`
	}
	targets := make([]targetView, 0, len(p.targetReader.targets))
	for _, target := range p.targetReader.targets {
		_, _, digest, err := target.snapshot()
		if err != nil {
			return nil, err
		}
		targets = append(targets, targetView{ReleaseDestination: pl.ReleaseDestination{TargetID: target.ID, TargetDigest: digest, Component: target.Component, Artifact: target.Artifact, Operation: "publish"}, Kind: "oci", Origin: target.Origin, Repository: target.Repository, CredentialConfigured: target.PasswordSecret != "" || target.BearerSecret != ""})
	}
	for _, target := range p.targetReader.files {
		_, _, digest, err := target.snapshot()
		if err != nil {
			return nil, err
		}
		targets = append(targets, targetView{ReleaseDestination: pl.ReleaseDestination{TargetID: target.ID, TargetDigest: digest, Component: target.Component, Artifact: target.Artifact, Operation: "publish"},
			Kind: "file", Origin: target.APIOrigin, Repository: target.Repository, Tag: target.Tag, SourceCommit: target.SourceCommit, ReleaseID: target.ReleaseID, AssetName: target.AssetName, CredentialConfigured: true})
	}
	slices.SortFunc(targets, func(a, b targetView) int { return strings.Compare(a.TargetID, b.TargetID) })
	return resultNode("candidate-configuration", "", map[string]any{"sources": sources, "targets": targets})
}

func (i *Integration) handleCandidatePrepare(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.configuredCandidate(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(args["candidate"])
	if err != nil {
		return nil, errors.New("candidate must be a JSON object")
	}
	var input pl.ReleaseCandidate
	if err := decodeCandidateObject(body, &input); err != nil {
		return nil, err
	}
	record, err := p.prepare(ctx, input)
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}

func (i *Integration) handleCandidateApprove(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.configuredCandidate(ctx)
	if err != nil {
		return nil, err
	}
	record, err := p.approve(ctx, asString(args["candidateId"]))
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}

func (i *Integration) handleCandidatePublish(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.configuredCandidate(ctx)
	if err != nil {
		return nil, err
	}
	record, err := p.publish(ctx, candidatePublishRequest{CandidateID: asString(args["candidateId"]), ApprovalID: asString(args["approvalId"]), TargetID: asString(args["targetId"]), Component: asString(args["component"]), Artifact: asString(args["artifact"])})
	if err != nil {
		return nil, err
	}
	return resultNode("publication", record.EffectID, record)
}

func (i *Integration) handleCandidateGet(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	record, err := p.ledger.get(ctx, asString(args["candidateId"]))
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}

func (i *Integration) handleCandidateRetire(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	record, err := p.retire(ctx, asString(args["candidateId"]))
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}
