package installation

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/znasllc-io/memql/component/pipelines"
)

// PublicationSource contains independently observed routing and a verified
// public release, never receiving credentials or a transferable write grant.
type PublicationSource struct {
	InstallationID, RepositoryURL, Path, CurrentRevision string
	Publication                                          pipelines.VerifiedPublishedRelease
}

func (p *Preparer) PublicationSource(ctx context.Context, installationID, candidateID, digest string) (PublicationSource, error) {
	var out PublicationSource
	ctx, err := p.selectionContext(ctx)
	if err != nil {
		return out, err
	}
	if !identifier.MatchString(installationID) || !artifactDigest.MatchString(candidateID) || !artifactDigest.MatchString(digest) {
		return out, errors.New("installation publication requires exact identities")
	}
	s, err := readReceiver(ctx, p.host.api, p.host.namespace, p.host.configurationName, p.host.catalog)
	if err != nil {
		return out, err
	}
	if s.configuration.InstallationID != installationID {
		return out, errors.New("publication names a different receiving installation")
	}
	var source struct {
		RepoURL  string `json:"repoURL"`
		Path     string `json:"path"`
		Revision string `json:"targetRevision"`
	}
	if json.Unmarshal(s.render.Source, &source) != nil {
		return out, errors.New("installation source is invalid")
	}
	publication, err := s.catalog.Get(ctx, s.configuration.Catalog.ID, candidateID, digest)
	if err != nil {
		return out, err
	}
	release, err := publication.Release()
	if err != nil || release.CandidateID != candidateID || publication.Digest() != digest {
		return out, errors.New("installation publication changed")
	}
	return PublicationSource{installationID, source.RepoURL, source.Path, source.Revision, publication}, nil
}
