package release

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/installation"
)

const InstallationOverlayVariable = "MEMQL_RELEASE_INSTALLATION_OVERLAY"
const overlayContract = "installation.overlay.v1"

var overlayRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

type OverlayAccess interface {
	GitHub() *githubapp.Client
	InstallationToken(context.Context, string, string, string, string) (string, int64, error)
	RepositoryWriterToken(context.Context, string, string, string, string) (string, int64, error)
}

type overlayBinding struct{ access OverlayAccess }
type overlayImage struct {
	Name       string `json:"name"`
	Component  string `json:"component"`
	Artifact   string `json:"artifact"`
	Repository string `json:"repository"`
}
type overlayConfiguration struct {
	FormatVersion       int            `json:"formatVersion"`
	InstallationID      string         `json:"installationId"`
	Repository          string         `json:"repository"`
	OwnerUserID         string         `json:"ownerUserId"`
	CredentialID        string         `json:"credentialId"`
	BaseBranch          string         `json:"baseBranch"`
	Path                string         `json:"path"`
	ProposalWorkflow    string         `json:"proposalWorkflow"`
	ObservationWorkflow string         `json:"observationWorkflow"`
	Images              []overlayImage `json:"images"`
	Files               []overlayFile  `json:"files,omitempty"`
}

type overlayRequest struct{ InstallationID, RequestID, CandidateID, CatalogDigest, ExpectedBaseRevision string }
type overlayRecord struct {
	ID            string                        `json:"id"`
	Owner         string                        `json:"owner"`
	Request       overlayRequest                `json:"request"`
	Configuration overlayConfiguration          `json:"configuration"`
	CreatedAt     time.Time                     `json:"createdAt"`
	Snapshot      *workflowhost.Snapshot        `json:"snapshot"`
	Replacement   githubapp.BranchReplacement   `json:"replacement"`
	Replacements  []githubapp.BranchReplacement `json:"replacements,omitempty"`
}
type InstallationProposal struct {
	ProposalID      string `json:"proposalId"`
	InstallationID  string `json:"installationId"`
	CandidateID     string `json:"candidateId"`
	CatalogDigest   string `json:"catalogDigest"`
	State           string `json:"state"`
	PullRequestURL  string `json:"pullRequestURL,omitempty"`
	OverlayRevision string `json:"overlayRevision,omitempty"`
}

func (i *Integration) ConfigureInstallationOverlay(access OverlayAccess) error {
	if i == nil || access == nil || !i.installationOverlay.CompareAndSwap(nil, &overlayBinding{access}) {
		return errors.New("installation overlay adapter is absent or already configured")
	}
	return nil
}
func overlayHash(value any) string {
	b, _ := json.Marshal(value)
	return string(id.NewUntracked().FromString(string(b)))
}
func overlayOwner(ctx context.Context) (string, error) {
	if err := catalogReader(ctx); err != nil {
		return "", err
	}
	actor, _ := auth.AccessFromContext(ctx)
	return memql.BareShortId(actor.UserId), nil
}
func (i *Integration) overlayConfig(ctx context.Context) (overlayConfiguration, error) {
	var c overlayConfiguration
	owner, err := overlayOwner(ctx)
	if err != nil {
		return c, err
	}
	if i.installation.Load() == nil || i.installationOverlay.Load() == nil || i.resolver.systemVariable == nil {
		return c, errors.New("installation overlay proposals are not configured")
	}
	raw, err := i.resolver.systemVariable(memql.ContextWithFreshRead(ctx), InstallationOverlayVariable)
	if err != nil || decodeCandidateObject([]byte(raw), &c) != nil || c.OwnerUserID != owner || validateOverlayConfiguration(c) != nil {
		return c, errors.New("installation overlay requires exact owner-bound operator configuration")
	}
	return c, nil
}
func (i *Integration) overlayDatabase() (*sql.DB, error) {
	var get func() *sql.DB
	if d := i.candidateDeps.Load(); d != nil {
		get = d.Database
	} else {
		get = i.candidateDB
	}
	if get == nil || get() == nil {
		return nil, errors.New("installation overlay journal unavailable")
	}
	return get(), nil
}
func (i *Integration) handleInstallationPropose(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cfg, err := i.overlayConfig(ctx)
	if err != nil {
		return nil, err
	}
	r := overlayRequest{asString(args["installationId"]), asString(args["requestId"]), asString(args["candidateId"]), asString(args["catalogDigest"]), asString(args["expectedBaseRevision"])}
	if r.InstallationID != cfg.InstallationID || !installationIdentifier.MatchString(r.RequestID) || !stringsHasDigest(r.CandidateID) || !stringsHasDigest(r.CatalogDigest) || !installationCommit.MatchString(r.ExpectedBaseRevision) {
		return nil, errors.New("installation proposal requires exact candidate, base and request identities")
	}
	db, err := i.overlayDatabase()
	if err != nil {
		return nil, err
	}
	s := &overlayScope{integration: i, db: db, configuration: cfg, request: r, owner: cfg.OwnerUserID}
	s.id = overlayHash([]string{s.owner, r.InstallationID, r.RequestID})
	prior, err := s.load(ctx, s.id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if prior.Record.Owner != s.owner || prior.Record.Request != r || overlayHash(prior.Record.Configuration) != overlayHash(cfg) {
			return nil, errors.New("installation proposal request was reused with changed input or authority")
		}
		if prior.PullRequest > 0 {
			return resultNode("installationProposal", s.id, prior.summary())
		}
		s.record = &prior.Record
		s.snapshot = prior.Record.Snapshot
	} else {
		s.snapshot, err = workflowhost.CaptureEntries(cfg.ProposalWorkflow, overlayContract, []string{cfg.ObservationWorkflow}, nil, s.operations())
		if err != nil {
			return nil, err
		}
	}
	_, err = workflowhost.RunSnapshot(ctx, s.snapshot, overlayContract, nil, workflowhost.Options{Operations: s.operations()})
	if err != nil {
		return nil, err
	}
	stored, err := s.load(ctx, s.id)
	if err != nil {
		return nil, err
	}
	if !s.proposed {
		return nil, errors.New("installation proposal recipe did not record a native proposal")
	}
	return resultNode("installationProposal", s.id, stored.summary())
}
func (i *Integration) handleInstallationProposalObserve(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	cfg, err := i.overlayConfig(ctx)
	if err != nil {
		return nil, err
	}
	key := asString(args["proposalId"])
	if !candidateArtifactDigest(key) {
		return nil, errors.New("installation proposal id is invalid")
	}
	db, err := i.overlayDatabase()
	if err != nil {
		return nil, err
	}
	s := &overlayScope{integration: i, db: db, configuration: cfg, owner: cfg.OwnerUserID, id: key}
	r, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	if r.Record.Owner != s.owner || overlayHash(r.Record.Configuration) != overlayHash(cfg) {
		return nil, errors.New("installation proposal configuration changed")
	}
	s.record = &r.Record
	s.request = r.Record.Request
	s.snapshot = r.Record.Snapshot
	// An observation has no native write-to-GitHub capability, even if a future
	// installed definition attempts to call the proposal phase.
	ops := map[string]workflowhost.Operation{"releaseOverlayObserve": s.observe}
	_, err = workflowhost.RunSnapshotEntry(ctx, s.snapshot, overlayContract, cfg.ObservationWorkflow, nil, workflowhost.Options{Operations: ops})
	if err != nil {
		return nil, err
	}
	r, err = s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	return resultNode("installationProposal", key, r.summary())
}

type overlayScope struct {
	integration   *Integration
	db            *sql.DB
	configuration overlayConfiguration
	request       overlayRequest
	owner, id     string
	snapshot      *workflowhost.Snapshot
	record        *overlayRecord
	source        *installation.PublicationSource
	replacements  []githubapp.BranchReplacement
	proposed      bool
}

func (s *overlayScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{"releaseOverlayRead": s.read, "releaseOverlayBuild": s.build, "releaseOverlayReserve": s.reserve, "releaseOverlayPropose": s.propose, "releaseOverlayObserve": s.observe}
}
func (s *overlayScope) gate(ctx context.Context, args map[string]any) error {
	owner, err := overlayOwner(ctx)
	if err != nil || owner != s.owner || len(args) != 0 {
		return errors.New("installation overlay operation is outside its admitted native scope")
	}
	cfg, err := s.integration.overlayConfig(ctx)
	if err != nil || overlayHash(cfg) != overlayHash(s.configuration) {
		return errors.New("installation overlay authority changed")
	}
	return ctx.Err()
}
func (s *overlayScope) client() (*githubapp.Client, error) {
	binding := s.integration.installationOverlay.Load()
	if binding == nil {
		return nil, errors.New("installation overlay adapter unavailable")
	}
	c := binding.access.GitHub()
	if c == nil {
		return nil, errors.New("installation overlay GitHub App unavailable")
	}
	return c, nil
}
func (s *overlayScope) token(ctx context.Context, write bool) (string, error) {
	if err := s.gate(ctx, nil); err != nil {
		return "", err
	}
	parts := strings.Split(s.configuration.Repository, "/")
	access := s.integration.installationOverlay.Load().access
	var token string
	var err error
	if write {
		token, _, err = access.RepositoryWriterToken(ctx, s.configuration.CredentialID, s.owner, parts[0], parts[1])
	} else {
		token, _, err = access.InstallationToken(ctx, s.configuration.CredentialID, s.owner, parts[0], parts[1])
	}
	if err != nil || token == "" {
		return "", errors.New("installation overlay repository grant is unavailable")
	}
	return token, nil
}
func (s *overlayScope) read(ctx context.Context, args map[string]any) (any, error) {
	if err := s.gate(ctx, args); err != nil {
		return nil, err
	}
	source, err := s.integration.installation.Load().PublicationSource(ctx, s.request.InstallationID, s.request.CandidateID, s.request.CatalogDigest)
	if err != nil {
		return nil, err
	}
	if err := validateOverlaySource(s.configuration, source); err != nil {
		return nil, err
	}
	s.source = &source
	return nil, nil
}
func (s *overlayScope) build(ctx context.Context, args map[string]any) (any, error) {
	if err := s.gate(ctx, args); err != nil {
		return nil, err
	}
	if s.source == nil {
		return nil, errors.New("installation overlay requires a verified receiving publication")
	}
	if err := validateOverlaySource(s.configuration, *s.source); err != nil {
		return nil, err
	}
	client, err := s.client()
	if err != nil {
		return nil, err
	}
	token, err := s.token(ctx, false)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(s.configuration.Repository, "/")
	if s.record == nil {
		head, _, err := client.BranchHead(ctx, token, parts[0], parts[1], s.configuration.BaseBranch)
		if err != nil || head != s.request.ExpectedBaseRevision {
			return nil, errors.New("installation overlay base moved since review")
		}
	}
	replacements := make([]githubapp.BranchReplacement, 0, len(s.configuration.files()))
	total := 0
	for _, file := range s.configuration.files() {
		before, err := client.FileContents(ctx, token, parts[0], parts[1], s.request.ExpectedBaseRevision, file.Path)
		if err != nil {
			return nil, fmt.Errorf("installation overlay source %s could not be read", file.Path)
		}
		content, err := buildOverlayFile(before, file, s.source.Publication)
		if err != nil {
			return nil, fmt.Errorf("installation overlay %s: %w", file.Path, err)
		}
		total += len(content)
		if total > 1<<20 {
			return nil, errors.New("installation overlay file set exceeds one MiB")
		}
		digest := sha256.Sum256(before)
		replacements = append(replacements, githubapp.BranchReplacement{Path: file.Path, BeforeSHA256: hex.EncodeToString(digest[:]), Content: string(content)})
	}
	if s.record != nil && !slices.Equal(s.record.files(), replacements) {
		return nil, errors.New("installation overlay proposal bytes changed")
	}
	s.replacements = replacements
	return nil, nil
}
func (s *overlayScope) propose(ctx context.Context, args map[string]any) (any, error) {
	if err := s.gate(ctx, args); err != nil {
		return nil, err
	}
	if s.record == nil || len(s.replacements) == 0 || !slices.Equal(s.replacements, s.record.files()) {
		return nil, errors.New("installation overlay proposal has no durable exact intent")
	}
	remote, err := s.client()
	if err != nil {
		return nil, err
	}
	token, err := s.token(ctx, true)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(s.configuration.Repository, "/")
	branch := "memql/update-" + s.id[:32]
	title := "Update installation to selected release"
	edit := githubapp.BranchChange{Branch: branch, ExpectedHead: s.request.ExpectedBaseRevision, OperationID: s.id, Title: title, Timestamp: s.record.CreatedAt, Create: true, Files: s.record.files(), Authorize: func(c context.Context, _ string) (string, error) { return s.token(c, true) }}
	commit, err := remote.ApplyBranchChange(ctx, token, parts[0], parts[1], edit)
	if err != nil {
		return nil, err
	}
	if !installationCommit.MatchString(commit) {
		return nil, errors.New("installation overlay branch has no verified commit")
	}
	if err = s.recordEffect(ctx, `UPDATE release_installation_proposals SET branch_commit=$2 WHERE proposal_id=$1 AND (branch_commit IS NULL OR branch_commit=$2) RETURNING proposal_id`, s.id, commit); err != nil {
		return nil, err
	}
	token, err = s.token(ctx, true)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("Pin the installation to signed candidate `%s` (catalog `%s`). The native preparation separately verifies the complete rendered diff, images, preserved data and identity. Merging prepares a selectable release; it does not install it.", s.request.CandidateID, s.request.CatalogDigest)
	url, err := remote.EnsureAuthorizedChangePullRequest(ctx, token, parts[0], parts[1], branch, s.configuration.BaseBranch, title, body, func(c context.Context) (string, error) { return s.token(c, true) })
	if err != nil {
		return nil, err
	}
	var number int
	if _, err = fmt.Sscanf(strings.TrimPrefix(url, "https://github.com/"+s.configuration.Repository+"/pull/"), "%d", &number); err != nil || number < 1 || url != fmt.Sprintf("https://github.com/%s/pull/%d", s.configuration.Repository, number) {
		return nil, errors.New("installation proposal pull request identity is invalid")
	}
	err = s.recordEffect(ctx, `UPDATE release_installation_proposals SET pull_request=$2,pull_request_url=$3 WHERE proposal_id=$1 AND (pull_request IS NULL OR (pull_request=$2 AND pull_request_url=$3)) RETURNING proposal_id`, s.id, number, url)
	s.proposed = err == nil
	return nil, err
}
func (s *overlayScope) observe(ctx context.Context, args map[string]any) (any, error) {
	if err := s.gate(ctx, args); err != nil {
		return nil, err
	}
	saved, err := s.load(ctx, s.id)
	if err != nil {
		return nil, err
	}
	if saved.PullRequest < 1 || !installationCommit.MatchString(saved.BranchCommit) {
		return nil, errors.New("installation proposal has no confirmed pull request")
	}
	remote, err := s.client()
	if err != nil {
		return nil, err
	}
	token, err := s.token(ctx, false)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(s.configuration.Repository, "/")
	pr, err := remote.PullRequestState(ctx, token, parts[0], parts[1], saved.PullRequest)
	if err != nil {
		return nil, err
	}
	if pr.HeadRepository != s.configuration.Repository || pr.BaseRepository != s.configuration.Repository || pr.HeadSHA != saved.BranchCommit || pr.HeadRef != "memql/update-"+s.id[:32] || pr.BaseRef != s.configuration.BaseBranch || pr.URL != saved.PullRequestURL {
		return nil, errors.New("installation proposal changed after its native commit")
	}
	if !pr.Merged {
		return map[string]any{"merged": false}, nil
	}
	if !installationCommit.MatchString(pr.MergeCommitSHA) {
		return nil, errors.New("installation proposal has no immutable merged revision")
	}
	for _, file := range saved.Record.files() {
		content, err := remote.FileContents(ctx, token, parts[0], parts[1], pr.MergeCommitSHA, file.Path)
		if err != nil || string(content) != file.Content {
			return nil, errors.New("merged installation overlay differs from reviewed proposal")
		}
	}
	err = s.recordEffect(ctx, `UPDATE release_installation_proposals SET merged_commit=$2 WHERE proposal_id=$1 AND (merged_commit IS NULL OR merged_commit=$2) RETURNING proposal_id`, s.id, pr.MergeCommitSHA)
	return map[string]any{"merged": err == nil}, err
}

func (s *overlayScope) recordEffect(ctx context.Context, query string, args ...any) error {
	var key string
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&key); err != nil {
		return fmt.Errorf("installation overlay effect receipt conflicts or could not be recorded: %w", err)
	}
	if key != s.id {
		return errors.New("installation overlay effect receipt identity changed")
	}
	return nil
}
