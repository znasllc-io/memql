package pipelinerun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/num"
)

const qualificationRefusal = "pipeline_qualification_refused"

var errQualificationCheckPending = errors.New("The initial qualification check is not yet observable; no work was dispatched and no replacement check was created.")

var qualificationRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// PullRequestQualification is immutable authority for ONE run, not a source
// subscription. Every mutable source/provider fact is checked again before a
// new effect. PolicyVersion binds the existing installed workflow snapshot;
// RequestDigest excludes it so retrying an uncertain request recovers the same
// attempt instead of acquiring whatever policy happens to be installed now.
type PullRequestQualification struct {
	Version        int               `json:"version"`
	RequestID      string            `json:"requestId"`
	RequestDigest  string            `json:"requestDigest"`
	PackageID      string            `json:"packageId"`
	CredentialID   string            `json:"credentialId"`
	OwnerUserID    string            `json:"ownerUserId"`
	AccountID      string            `json:"accountId"`
	Repository     string            `json:"repository"`
	DefaultBranch  string            `json:"defaultBranch"`
	Name           string            `json:"name"`
	InstallationID int64             `json:"installationId,string"`
	RepositoryID   int64             `json:"repositoryId,string"`
	AppID          int64             `json:"appId,string"`
	PullRequest    int               `json:"pullRequest"`
	HeadSHA        string            `json:"headSha"`
	HeadBranch     string            `json:"headBranch"`
	BaseSHA        string            `json:"baseSha"`
	ManifestDigest string            `json:"manifestDigest"`
	Compute        pipelines.Compute `json:"compute"`
	SecretNames    []string          `json:"secretNames"`
	ExpiresAt      time.Time         `json:"expiresAt"`
	PolicyVersion  string            `json:"policyVersion"`
}

type QualifyPullRequestRequest struct {
	PackageID, RequestID             string
	PullRequest                      int
	ExpectedHeadSHA, ExpectedBaseSHA string
	ExpectedManifestDigest           string
	Compute                          pipelines.Compute
	SecretNames                      []string
	ExpiresAt                        time.Time
}

type PullRequestPreview struct {
	PreviewResult
	PullRequest                         int
	BaseSHA, HeadBranch, ManifestDigest string
}

type pullRequestInspection struct {
	inspection
	token        string
	pr           githubapp.PullRequestHead
	digest       string
	repositoryID int64
}

func qualificationError(detail string) error {
	return pipelines.Refuse(qualificationRefusal, "qualification", "%s", detail)
}

func validQualificationSHA(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(value) == 40 && len(data) == 20 && value == strings.ToLower(value)
}

func validQualificationDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func manifestDigest(tree fs.FS) (string, error) {
	data, err := fs.ReadFile(tree, pipelines.ManifestPath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (q PullRequestQualification) digest() string {
	q.RequestDigest, q.PolicyVersion = "", ""
	return reviewDigest(q)
}

func (q PullRequestQualification) object() map[string]any {
	b, _ := json.Marshal(q)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func readQualification(value map[string]any) (PullRequestQualification, error) {
	var q PullRequestQualification
	if err := decodeSelectionObject(value, &q); err != nil || q.Version != 1 ||
		!qualificationRequestID.MatchString(q.RequestID) || q.RequestDigest != q.digest() ||
		q.PackageID == "" || q.CredentialID == "" || auth.NamesNoPerson(q.OwnerUserID) ||
		q.Repository == "" || q.DefaultBranch == "" || q.Name == "" || q.RepositoryID <= 0 || q.InstallationID <= 0 || q.AppID <= 0 || q.PullRequest <= 0 ||
		!validQualificationSHA(q.HeadSHA) || !validQualificationSHA(q.BaseSHA) || !pipelines.ValidBranchName(q.HeadBranch) ||
		!validQualificationDigest(q.ManifestDigest) || q.PolicyVersion == "" || q.ExpiresAt.IsZero() {
		return q, qualificationError("The saved pull-request qualification is missing or changed.")
	}
	return q, nil
}

func qualificationRunID(pipelineID, requestID string) string {
	return deriveID("pipelines.qualification", bareID(pipelineID)+"\x00"+requestID)
}

// ValidateQualification is the local binding fence used by the driver and the
// independent-review adapter. It confers no provider authority: callers must
// also verify current source ownership, grant and PR facts before effects.
func ValidateQualification(p Pipeline, r Run, now time.Time) error {
	q, err := readQualification(r.Qualification)
	if err != nil {
		return err
	}
	if p.Status != PipelineQualification || !q.ExpiresAt.After(now) {
		return qualificationError("The pull-request qualification expired or its source was revoked.")
	}
	return validateQualificationBindings(p, r, q)
}

// Local identity remains necessary when reporting a stopped/expired attempt.
// It does not require live execution permission to report failure or cancellation.
func validateQualificationBindings(p Pipeline, r Run, q PullRequestQualification) error {
	if r.Trigger != TriggerQualification ||
		r.Event != pipelines.EventPullRequest || r.Mode != pipelines.ModeFull || r.RerunOf != "" || r.RerunFailedOnly ||
		r.ID != qualificationRunID(p.ID, q.RequestID) || r.PipelineID != p.ID ||
		!sameID(q.OwnerUserID, p.OwnerUserID) || !sameID(r.OwnerUserID, p.OwnerUserID) ||
		q.PackageID != p.PackageID || q.CredentialID != p.CredentialID || q.AccountID != p.AccountID || r.AccountID != q.AccountID ||
		q.Repository != p.Repository || r.Repository != p.Repository || q.DefaultBranch != p.DefaultBranch ||
		q.Name != p.Name || q.InstallationID != p.InstallationID || q.Compute != p.Compute || !slices.Equal(q.SecretNames, p.SecretNames) ||
		q.HeadSHA != r.SHA || q.BaseSHA != r.BaseSHA || q.HeadBranch != r.HeadBranch || q.PullRequest != r.PullRequest {
		return qualificationError("The pull-request qualification source, run or approved scope changed.")
	}
	snapshot, err := readPipelinePolicy(r.PolicySnapshot)
	if err != nil || snapshot.Version != q.PolicyVersion {
		return qualificationError("The qualification's saved policy changed.")
	}
	return nil
}

func (i *Integration) inspectPullRequest(ctx context.Context, d Deps, caller, packageID string, number int) (pullRequestInspection, error) {
	var out pullRequestInspection
	if number <= 0 || strings.TrimSpace(packageID) == "" {
		return out, qualificationError("A source and positive pull request number are required.")
	}
	pkg, err := d.Store.PackageForCaller(memql.ContextWithFreshRead(ctx), packageID)
	if err != nil {
		return out, err
	}
	if pkg == nil {
		return out, &packages.Refusal{Code: packages.CodeSourceUnreadable, Detail: "The source is not readable by you."}
	}
	if !sameID(pkg.OwnerUserID, caller) {
		return out, ErrNotOwner
	}
	if pkg.Status != "" && pkg.Status != "active" || pkg.SourceKind != "repo" || pkg.CredentialID == "" {
		return out, qualificationError("Qualification needs an active repository source with its owner's GitHub connection.")
	}
	out.pkg = pkg
	out.repository, err = githubRepository(pkg.RepoURL)
	if err != nil {
		return out, err
	}
	if refusal, err := alreadyConnected(ctx, d, out.repository, pkg.ID); err != nil || refusal != nil {
		return out, firstErr(err, refusal)
	}
	out.token, out.installationID, err = d.GitHub.InstallationToken(ctx, pkg.CredentialID, pkg.OwnerUserID, out.repository)
	if err != nil {
		return out, err
	}
	if out.installationID <= 0 {
		return out, qualificationError("The source's current GitHub installation could not be verified.")
	}
	info, err := d.GitHub.Repository(ctx, out.token, out.repository)
	if err != nil {
		return out, err
	}
	out.branch = strings.TrimSpace(info.DefaultBranch)
	if info.ID <= 0 || out.branch == "" || !strings.EqualFold(info.FullName, out.repository) {
		return out, qualificationError("The provider repository or default branch could not be verified.")
	}
	out.repositoryID = info.ID
	out.pr, err = d.GitHub.PullRequestHead(ctx, out.token, out.repository, number)
	if err != nil {
		return out, err
	}
	if err := validateQualificationPR(out.pr, out.repository, out.branch, number); err != nil {
		return out, err
	}
	out.head = out.pr.HeadSHA
	tree, err := d.GitHub.Tree(ctx, out.token, out.repository, out.head, func(p string) bool { return p == pipelines.ManifestPath }, maxManifestBytes)
	if err != nil {
		return out, err
	}
	out.digest, err = manifestDigest(tree)
	if err != nil {
		return out, err
	}
	out.manifest, err = packages.ReadManifest(tree)
	if err != nil {
		return out, err
	}
	if out.manifest.Pipeline == nil {
		return out, pipelines.Refuse(pipelines.CodeNotDeclared, "", "The pull request declares no pipeline.")
	}
	if refusal := pipelines.Validate(out.manifest.Pipeline); refusal != nil {
		return out, refusal
	}
	return out, nil
}

func validateQualificationPR(pr githubapp.PullRequestHead, repository, defaultBranch string, number int) error {
	if pr.Number != number || pr.State != "open" || !strings.EqualFold(pr.HeadRepository, repository) || !strings.EqualFold(pr.BaseRepository, repository) ||
		pr.BaseRef != defaultBranch || !validQualificationSHA(pr.HeadSHA) || !validQualificationSHA(pr.BaseSHA) || !pipelines.ValidBranchName(pr.HeadRef) {
		return qualificationError("Qualification requires an open pull request from this repository into its current default branch.")
	}
	return nil
}

func (i *Integration) PreviewPullRequest(ctx context.Context, packageID string, number int) (PullRequestPreview, error) {
	caller, err := personFrom(ctx)
	if err != nil {
		return PullRequestPreview{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return PullRequestPreview{}, errNoStore
	}
	if d.GitHub == nil {
		return PullRequestPreview{}, errNoGitHub
	}
	insp, err := i.inspectPullRequest(ctx, d, caller, packageID, number)
	res := PullRequestPreview{PullRequest: number, BaseSHA: insp.pr.BaseSHA, HeadBranch: insp.pr.HeadRef, ManifestDigest: insp.digest}
	res.Repository, res.DefaultBranch, res.SHA = insp.repository, insp.branch, insp.head
	if err != nil {
		if refusal, ok := previewRefusalOf(err); ok {
			res.Refusal = refusal
			return res, nil
		}
		return res, err
	}
	populatePreview(&res.PreviewResult, insp.manifest)
	return res, nil
}

func (i *Integration) handlePreviewPullRequest(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.PreviewPullRequest(handlerContext(ctx), stringArg(args, "packageId"), qualificationNumber(args["pullRequest"]))
	if err != nil {
		return nil, err
	}
	value := res.payload()
	value["headSha"], value["baseSha"], value["headBranch"], value["manifestDigest"], value["pullRequest"] = res.SHA, res.BaseSHA, res.HeadBranch, res.ManifestDigest, res.PullRequest
	return resultNode(value), nil
}

func (i *Integration) handleQualifyPullRequest(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	expires, err := time.Parse(time.RFC3339Nano, stringArg(args, "expiresAt"))
	if err != nil {
		return nil, qualificationError("expiresAt must be an explicit future timestamp.")
	}
	names, _ := stringListArg(args, "secretNames")
	r, err := i.QualifyPullRequest(handlerContext(ctx), QualifyPullRequestRequest{PackageID: stringArg(args, "packageId"), RequestID: stringArg(args, "requestId"), PullRequest: qualificationNumber(args["pullRequest"]), ExpectedHeadSHA: stringArg(args, "expectedHeadSha"), ExpectedBaseSHA: stringArg(args, "expectedBaseSha"), ExpectedManifestDigest: stringArg(args, "expectedManifestDigest"), Compute: pipelines.Compute(stringArg(args, "compute")), SecretNames: names, ExpiresAt: expires})
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"runId": r.ID, "pipelineId": r.PipelineID, "attempt": r.Attempt, "status": r.Status, "headSha": r.SHA, "baseSha": r.BaseSHA, "pullRequest": r.PullRequest}), nil
}

func (i *Integration) QualifyPullRequest(ctx context.Context, req QualifyPullRequestRequest) (Run, error) {
	caller, err := personFrom(ctx)
	if err != nil {
		return Run{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return Run{}, errNoStore
	}
	if d.GitHub == nil {
		return Run{}, errNoGitHub
	}
	if !qualificationRequestID.MatchString(req.RequestID) || !req.ExpiresAt.After(d.now()) || req.ExpiresAt.Year() > 9999 {
		return Run{}, qualificationError("Qualification needs a stable requestId and explicit finite future expiry.")
	}
	if !validQualificationSHA(req.ExpectedHeadSHA) || !validQualificationSHA(req.ExpectedBaseSHA) || !validQualificationDigest(req.ExpectedManifestDigest) {
		return Run{}, qualificationError("Qualification needs the preview's exact head, base and manifest digest.")
	}
	compute := req.Compute
	if compute == "" {
		compute = pipelines.ComputeCluster
	}
	if compute != pipelines.ComputeCluster && compute != pipelines.ComputeClusterAndFleet {
		return Run{}, qualificationError("The compute scope is invalid.")
	}
	names, refusal := allowlist(req.SecretNames)
	if refusal != nil {
		return Run{}, refusal
	}
	insp, err := i.inspectPullRequest(ctx, d, caller, req.PackageID, req.PullRequest)
	if err != nil {
		return Run{}, err
	}
	if insp.head != req.ExpectedHeadSHA || insp.pr.BaseSHA != req.ExpectedBaseSHA || insp.digest != req.ExpectedManifestDigest {
		return Run{}, qualificationError("The pull request or manifest changed since preview; review the current proposal first.")
	}
	if refusal := pipelines.Consent(insp.manifest.Pipeline, compute, names); refusal != nil {
		return Run{}, refusal
	}
	writer, appID, err := qualificationCheckAuthority(ctx, d, insp.pkg.CredentialID, insp.pkg.OwnerUserID, insp.repository)
	if err != nil {
		return Run{}, err
	}
	p := Pipeline{ID: PipelineIDFor(insp.pkg.ID), OwnerUserID: insp.pkg.OwnerUserID, AccountID: insp.pkg.AccountID, PackageID: insp.pkg.ID, CredentialID: insp.pkg.CredentialID, Repository: insp.repository, InstallationID: insp.installationID, DefaultBranch: insp.branch, Name: insp.manifest.Name, Delivery: DeliveryWebhook, Compute: compute, SecretNames: names, Status: PipelineQualification}
	q := PullRequestQualification{Version: 1, RequestID: req.RequestID, PackageID: p.PackageID, CredentialID: p.CredentialID, OwnerUserID: p.OwnerUserID, AccountID: p.AccountID, Repository: p.Repository, InstallationID: p.InstallationID, DefaultBranch: p.DefaultBranch, Name: p.Name, PullRequest: req.PullRequest, HeadSHA: insp.head, BaseSHA: insp.pr.BaseSHA, HeadBranch: insp.pr.HeadRef, ManifestDigest: insp.digest, Compute: compute, SecretNames: names, ExpiresAt: req.ExpiresAt.UTC()}
	q.AppID, q.RepositoryID = appID, insp.repositoryID
	q.RequestDigest = q.digest()
	// Reusing a request identity never renews authority or accepts changed input.
	if prior, err := d.Store.RunByID(memql.ContextWithFreshRead(ctx), qualificationRunID(p.ID, q.RequestID)); err != nil {
		return Run{}, err
	} else if prior != nil {
		old, err := readQualification(prior.Qualification)
		if err != nil || old.RequestDigest != q.RequestDigest {
			return Run{}, qualificationError("requestId already names a different qualification; it cannot authorize changed input.")
		}
		current, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), p.ID)
		if err != nil {
			return Run{}, err
		}
		if current == nil {
			return Run{}, qualificationError("The qualification pipeline no longer exists.")
		}
		if err := ValidateQualification(*current, *prior, d.now()); err != nil {
			return Run{}, err
		}
		return *prior, nil
	}
	// Never turn an ordinary owner shutdown into new permission. A qualification
	// source is created atomically inactive, and remains a separate lifecycle
	// until the owner uses the ordinary default-branch Connect path.
	err = d.gate(ctx, RepositoryGateKey(p.Repository), func(gctx context.Context) error {
		fresh := memql.ContextWithFreshRead(gctx)
		if refusal, err := alreadyConnected(fresh, d, p.Repository, p.PackageID); err != nil || refusal != nil {
			return firstErr(err, refusal)
		}
		if err := qualificationSourceFence(fresh, d, p, q); err != nil {
			return err
		}
		existing, err := d.Store.PipelineByID(fresh, p.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Status != PipelineQualification {
				return qualificationError("This source already has an ordinary pipeline; qualification cannot replace or reactivate it.")
			}
			if !qualificationPipelineEqual(*existing, p) {
				return qualificationError("This qualification pipeline has a different source or consent scope.")
			}
			return nil
		}
		return d.Store.CreatePipeline(gctx, p)
	})
	if err != nil {
		return Run{}, err
	}
	opened, err := i.openWithToken(ctx, d, p, Opening{Event: pipelines.EventPullRequest, Mode: pipelines.ModeFull, SHA: q.HeadSHA, BaseSHA: q.BaseSHA, Branch: q.HeadBranch, PullRequest: q.PullRequest, Title: insp.pr.Title, Trigger: TriggerQualification, Qualification: &q, beforeCreate: func(gctx context.Context) error {
		fresh := memql.ContextWithFreshRead(gctx)
		current, err := d.Store.PipelineByID(fresh, p.ID)
		if err != nil {
			return err
		}
		if current == nil || current.Status != PipelineQualification || !qualificationPipelineEqual(*current, p) || !q.ExpiresAt.After(d.now()) {
			return qualificationError("The qualification source changed before the run could open.")
		}
		return qualificationSourceFence(fresh, d, p, q)
	}}, writer)
	return opened.Run, err
}

func qualificationPipelineEqual(a, b Pipeline) bool {
	return a.ID == b.ID && sameID(a.OwnerUserID, b.OwnerUserID) && a.AccountID == b.AccountID && a.PackageID == b.PackageID && a.CredentialID == b.CredentialID && a.Repository == b.Repository && a.DefaultBranch == b.DefaultBranch && a.Name == b.Name && a.InstallationID == b.InstallationID && a.Compute == b.Compute && slices.Equal(a.SecretNames, b.SecretNames)
}

func qualificationSourceFence(ctx context.Context, d Deps, p Pipeline, q PullRequestQualification) error {
	pkg, err := d.Store.PackageForCaller(memql.ContextWithFreshRead(auth.ContextWithUserActor(ctx, q.OwnerUserID)), q.PackageID)
	if err != nil {
		return err
	}
	if pkg == nil || !sameID(pkg.OwnerUserID, q.OwnerUserID) || pkg.AccountID != q.AccountID || pkg.CredentialID != q.CredentialID || pkg.SourceKind != "repo" || pkg.Status != "" && pkg.Status != "active" {
		return qualificationError("The qualification source is no longer owned, active or bound to its approved connection.")
	}
	repository, err := githubRepository(pkg.RepoURL)
	if err != nil || repository != q.Repository || repository != p.Repository {
		return qualificationError("The qualification repository changed.")
	}
	return nil
}

// qualificationFence is deliberately outside row gates: provider calls must
// not monopolize the shared lock pool. Each executable step and final success
// rechecks it; recovery does not inherit an earlier process's observation.
func (dr *runDriver) qualificationFence(ctx context.Context) error {
	if dr.run.Qualification == nil && dr.run.Trigger != TriggerQualification {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()
	r, p, err := dr.qualificationLocalFence(ctx)
	if err != nil {
		return err
	}
	q, _ := readQualification(r.Qualification)
	_, token, err := qualificationProviderFence(ctx, dr.d, *p, q)
	if err != nil {
		return err
	}
	provider, ok := dr.d.GitHub.(qualificationGitHub)
	if !ok {
		return qualificationError("The qualification check observer is unavailable.")
	}
	checkID, err := provider.FindQualificationCheck(ctx, token, p.Repository, r.SHA, pipelines.CheckRunName(q.Name), r.ID, q.AppID)
	if err != nil {
		return err
	}
	if checkID <= 0 {
		return errQualificationCheckPending
	}
	if r.CheckRunID > 0 && checkID != r.CheckRunID {
		return qualificationError("The qualification's provider check receipt changed.")
	}
	// Provider reads may block. Repeat local revocation, cancellation and
	// expiry fences after them before handing any new effect to its worker.
	_, freshPipeline, err := dr.qualificationLocalFence(ctx)
	if err != nil {
		return err
	}
	return qualificationSourceFence(ctx, dr.d, *freshPipeline, q)
}

func (dr *runDriver) qualificationLocalFence(ctx context.Context) (*Run, *Pipeline, error) {
	r, err := dr.d.Store.RunByID(memql.ContextWithFreshRead(ctx), dr.run.ID)
	if err != nil {
		return nil, nil, err
	}
	if r == nil || r.CancelRequested {
		return nil, nil, qualificationError("The qualification run is absent or cancelled.")
	}
	p, err := dr.d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), r.PipelineID)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, nil, qualificationError("The qualification pipeline no longer exists.")
	}
	if err := ValidateQualification(*p, *r, dr.d.now()); err != nil {
		return nil, nil, err
	}
	if !reflect.DeepEqual(r.Qualification, dr.run.Qualification) {
		return nil, nil, qualificationError("The qualification authority changed during execution.")
	}
	return r, p, nil
}

// qualificationProviderFence refreshes provider identity immediately before an
// opening or dispatch. It performs no writes and must run outside all row gates.
func qualificationProviderFence(ctx context.Context, d Deps, p Pipeline, q PullRequestQualification) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()
	if !q.ExpiresAt.After(d.now()) {
		return "", "", qualificationError("The qualification expired before its next effect.")
	}
	if err := qualificationSourceFence(ctx, d, p, q); err != nil {
		return "", "", err
	}
	token, installation, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return "", "", err
	}
	if installation != q.InstallationID {
		return "", "", qualificationError("The qualification GitHub installation changed.")
	}
	writer, appID, err := qualificationCheckAuthority(ctx, d, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return "", "", err
	}
	if appID != q.AppID {
		return "", "", qualificationError("The qualification GitHub App changed.")
	}
	info, err := d.GitHub.Repository(ctx, token, p.Repository)
	if err != nil {
		return "", "", err
	}
	if info.ID != q.RepositoryID || !strings.EqualFold(info.FullName, p.Repository) || info.DefaultBranch != q.DefaultBranch {
		return "", "", qualificationError("The qualification repository or default branch changed.")
	}
	pr, err := d.GitHub.PullRequestHead(ctx, token, p.Repository, q.PullRequest)
	if err != nil {
		return "", "", err
	}
	if err := validateQualificationPR(pr, p.Repository, q.DefaultBranch, q.PullRequest); err != nil {
		return "", "", err
	}
	if pr.HeadSHA != q.HeadSHA || pr.BaseSHA != q.BaseSHA || pr.HeadRef != q.HeadBranch {
		return "", "", qualificationError("The qualified pull request's head or base changed; review its current proposal.")
	}
	policy, err := capturePipelinePolicy(ctx, d, p, Opening{Event: pipelines.EventPullRequest, Mode: pipelines.ModeFull, SHA: q.HeadSHA, BaseSHA: q.BaseSHA, Branch: q.HeadBranch, PullRequest: q.PullRequest, Trigger: TriggerQualification})
	if err != nil {
		return "", "", err
	}
	if q.PolicyVersion != "" && policy.Version != q.PolicyVersion {
		return "", "", qualificationError("The installed qualification policy changed.")
	}
	if !q.ExpiresAt.After(d.now()) {
		return "", "", qualificationError("The qualification expired during provider observation.")
	}
	if err := qualificationSourceFence(ctx, d, p, q); err != nil {
		return "", "", err
	}
	return token, writer, nil
}

// qualificationCheckDestination allows reporting a stopped attempt only to its
// original provider repository/App through the source owner's still-live grant.
// Failure reporting does not renew execution permission or require an open PR.
func qualificationCheckDestination(ctx context.Context, d Deps, p Pipeline, r Run) (string, error) {
	q, err := readQualification(r.Qualification)
	if err != nil {
		return "", err
	}
	if err := validateQualificationBindings(p, r, q); err != nil {
		return "", err
	}
	if err := qualificationSourceFence(ctx, d, p, q); err != nil {
		return "", err
	}
	token, installation, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return "", err
	}
	writer, appID, err := qualificationCheckAuthority(ctx, d, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return "", err
	}
	info, err := d.GitHub.Repository(ctx, token, p.Repository)
	if err != nil {
		return "", err
	}
	if installation != q.InstallationID || appID != q.AppID || info.ID != q.RepositoryID || !strings.EqualFold(info.FullName, q.Repository) {
		return "", qualificationError("The qualification check destination or provider authority changed.")
	}
	if err := qualificationSourceFence(ctx, d, p, q); err != nil {
		return "", err
	}
	return writer, nil
}

// QualificationSourceAllowed is for adapters that already carry a run-bound
// fence. The pure check never substitutes for their fresh provider checks.
func QualificationSourceAllowed(p Pipeline, r Run, now time.Time) bool {
	return p.Active() && r.Qualification == nil && r.Trigger != TriggerQualification || ValidateQualification(p, r, now) == nil
}

func qualificationNumber(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return num.Int64OrZero(v)
	case float64:
		n, ok := num.WholeInt64(v)
		if ok {
			return num.Int64OrZero(n)
		}
	}
	return 0
}

type qualificationGitHub interface {
	QualificationCheckAuthority(context.Context, string, string, string) (string, int64, error)
	FindQualificationCheck(context.Context, string, string, string, string, string, int64) (int64, error)
}

func qualificationCheckAuthority(ctx context.Context, d Deps, credential, owner, repository string) (string, int64, error) {
	provider, ok := d.GitHub.(qualificationGitHub)
	if !ok {
		return "", 0, qualificationError("The provider cannot verify qualification check authority.")
	}
	token, appID, err := provider.QualificationCheckAuthority(ctx, credential, owner, repository)
	if err == nil && (token == "" || appID <= 0) {
		err = qualificationError("The provider returned no verifiable qualification check authority.")
	}
	return token, appID, err
}
