package release

import (
	"context"
	"errors"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/core/common"
)

type overlayOwnerAccess interface {
	RepositoryOwnerWriterToken(context.Context, string, string, string, string) (string, error)
}

func (s *overlayScope) ownerRepository(ctx context.Context) (githubapp.RepositoryInfo, error) {
	if err := s.gate(ctx, nil); err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	remote, err := s.client()
	if err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	token, err := s.token(ctx, false)
	if err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	parts := strings.Split(s.configuration.Repository, "/")
	info, err := remote.Repository(ctx, token, parts[0], parts[1])
	if err != nil {
		return info, err
	}
	if info.ID <= 0 || !strings.EqualFold(info.FullName, s.configuration.Repository) || info.DefaultBranch != s.configuration.BaseBranch || info.HasPullRequests == nil || *info.HasPullRequests || s.configuration.LandingMode != "owner_push" {
		return info, errors.New("installation repository does not permit the configured owner landing")
	}
	if s.record != nil && s.record.RepositoryID != info.ID {
		return info, errors.New("installation repository identity changed")
	}
	return info, nil
}

// This command is deliberately absent from overlay recipe operations. Preparing
// or observing a proposal cannot silently become permission to move main.
func (i *Integration) handleInstallationProposalLand(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, running := common.RunFromContext(ctx); running {
		return nil, errors.New("installation owner landing requires a separate interactive command")
	}
	cfg, err := i.overlayConfig(ctx)
	if err != nil {
		return nil, err
	}
	key, expected := asString(args["proposalId"]), asString(args["expectedCommit"])
	if cfg.LandingMode != "owner_push" || !candidateArtifactDigest(key) || !installationCommit.MatchString(expected) {
		return nil, errors.New("confirm the exact owner-push installation proposal")
	}
	db, err := i.overlayDatabase()
	if err != nil {
		return nil, err
	}
	s := &overlayScope{integration: i, db: db, configuration: cfg, owner: cfg.OwnerUserID, id: key}
	saved, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	if overlayHash(saved.Record.Configuration) != overlayHash(cfg) || saved.BranchCommit != expected {
		return nil, errors.New("installation proposal or its configuration changed")
	}
	if saved.LandedCommit == expected {
		return resultNode("installationProposal", key, saved.summary())
	}
	s.record = &saved.Record
	s.request = saved.Record.Request
	remote, err := s.client()
	if err != nil {
		return nil, err
	}
	access, ok := i.installationOverlay.Load().access.(overlayOwnerAccess)
	if !ok {
		return nil, errors.New("installation owner connection is unavailable")
	}
	parts := strings.Split(cfg.Repository, "/")
	refresh := func(c context.Context) (string, error) {
		if _, e := s.ownerRepository(c); e != nil {
			return "", e
		}
		return access.RepositoryOwnerWriterToken(c, cfg.CredentialID, cfg.OwnerUserID, parts[0], parts[1])
	}
	token, err := refresh(ctx)
	if err != nil {
		return nil, err
	}
	if err = remote.LandChange(ctx, token, parts[0], parts[1], cfg.BaseBranch, s.request.ExpectedBaseRevision, expected, refresh); err != nil {
		return nil, err
	}
	// Duplicate confirmations are harmless: the provider CAS and this immutable
	// receipt accept only the same single-parent commit, across all replicas.
	if err = s.recordEffect(ctx, `UPDATE release_installation_proposals SET landed_commit=$2 WHERE proposal_id=$1 AND (landed_commit IS NULL OR landed_commit=$2) RETURNING proposal_id`, key, expected); err != nil {
		return nil, err
	}
	saved, err = s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	return resultNode("installationProposal", key, saved.summary())
}
