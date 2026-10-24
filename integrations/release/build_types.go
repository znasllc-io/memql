package release

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

const buildContract = "release-build-v1"
const buildPlanContract = "release-build-plan-v1"
const buildPublicationContract = "release-build-publication-v1"

// ReleaseBuildPipeline is supplied by app wiring. No public argument or stored
// configuration can supply a provider client, grant, or execution receipt.
type ReleaseBuildPipeline interface {
	InspectReleaseBuild(context.Context, pl.ReleaseBuildRequest) (pl.ReleaseBuildSource, error)
	StartReleaseBuild(context.Context, pl.ReleaseBuildRequest, func(context.Context) error) (pl.ReleaseBuildRun, error)
	ObserveReleaseBuild(context.Context, pl.ReleaseBuildRequest) (pl.ReleaseBuildRun, bool, error)
}

type releaseBuildBinding struct{ pipeline ReleaseBuildPipeline }

func (i *Integration) ConfigureReleaseBuilds(pipeline ReleaseBuildPipeline) error {
	if i == nil || pipeline == nil || !i.releaseBuild.CompareAndSwap(nil, &releaseBuildBinding{pipeline}) {
		return errors.New("release build pipeline must be configured exactly once")
	}
	return nil
}

// The first version deliberately handles one component/run. Multi-source
// assembly remains available independently; it cannot guess a tag authority.
type candidateBuildConfiguration struct {
	Component           string `json:"component"`
	Run                 string `json:"run"`
	Branch              string `json:"branch"`
	TagPrefix           string `json:"tagPrefix"`
	TagTargetID         string `json:"tagTargetId"`
	Plan                string `json:"plan"`
	Workflow            string `json:"workflow"`
	PublicationWorkflow string `json:"publicationWorkflow"`
}

func (p candidateProfile) validateBuild() error {
	b := p.Build
	if b == nil {
		return nil
	}
	if len(p.Components) != 1 || p.Components[0].Component != b.Component || len(p.Components[0].Runs) != 1 || p.Components[0].Runs[0] != b.Run ||
		!pl.ValidBranchName(b.Branch) || !candidateJSONField.MatchString(b.Plan) || !candidateJSONField.MatchString(b.Workflow) || !candidateJSONField.MatchString(b.PublicationWorkflow) ||
		!candidateTargetName.MatchString(b.TagTargetID) || len(b.TagPrefix) > 128 || !pl.ValidBranchName(b.TagPrefix+"1.2.3") {
		return errors.New("release build requires one declared component/run, branch and installed recipes")
	}
	matched := false
	for _, target := range p.ReleaseAssets {
		if target.Component == b.Component {
			if target.Repository != p.Components[0].Repository || target.TagPrefix != b.TagPrefix {
				return errors.New("release build tag authority differs from its publication source")
			}
			matched = matched || target.ID == b.TagTargetID
		}
	}
	if !matched {
		return errors.New("release build requires an explicitly configured tag/publication authority")
	}
	return nil
}

type releaseBuildPlan struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
}
type releaseBuildSelection struct {
	Owner         string                 `json:"owner"`
	ProfileName   string                 `json:"profileName"`
	Configuration string                 `json:"configuration"`
	Request       pl.ReleaseBuildRequest `json:"request"`
	Source        pl.ReleaseBuildSource  `json:"source"`
	Version       string                 `json:"version"`
	Tag           string                 `json:"tag"`
	Component     string                 `json:"component"`
	RunAlias      string                 `json:"runAlias"`
	Plan          *workflowhost.Snapshot `json:"plan"`
	Workflow      *workflowhost.Snapshot `json:"workflow"`
	Publication   *workflowhost.Snapshot `json:"publication"`
}

type releaseBuildRecord struct {
	ID                 string                `json:"intentId"`
	SelectionDigest    string                `json:"selectionDigest"`
	Selection          releaseBuildSelection `json:"selection"`
	Phase              string                `json:"phase"`
	ReasonCode         string                `json:"reasonCode,omitempty"`
	Reason             string                `json:"reason,omitempty"`
	TagAttempted       bool                  `json:"tagAttempted"`
	TagConfirmed       bool                  `json:"tagConfirmed"`
	RunAttempted       bool                  `json:"runAttempted"`
	Run                *pl.ReleaseBuildRun   `json:"run,omitempty"`
	CandidateID        string                `json:"candidateId,omitempty"`
	ApprovalID         string                `json:"approvalId,omitempty"`
	PublicationStarted bool                  `json:"publicationStarted"`
	CreatedAt          time.Time             `json:"createdAt"`
	UpdatedAt          time.Time             `json:"updatedAt"`
}

func (r releaseBuildRecord) validate(owner, key string) error {
	identity, err := buildRequestID(r.Selection)
	digest, digestErr := profileDigest(r.Selection)
	if err != nil || digestErr != nil || digest != r.SelectionDigest || identity != key || r.ID != key || r.Selection.Owner != owner || !candidateSourceCommit(r.Selection.Request.ExpectedSHA) || r.Selection.Tag != r.Selection.Request.Tag || r.Selection.Version == "" {
		return errors.New("release build journal identity is invalid")
	}
	s := r.Selection
	if s.Source.OwnerUserID != owner || s.Source.PipelineID != s.Request.PipelineID || s.Source.SourceID != s.Request.SourceID || s.Source.CredentialID != s.Request.CredentialID || s.Source.InstallationID != s.Request.InstallationID || s.Source.RepositoryID != s.Request.RepositoryID || s.Source.Repository != s.Request.Repository || s.Source.Branch != s.Request.Branch || s.Source.SHA != s.Request.ExpectedSHA || s.Request.SourceID == "" || s.Request.CredentialID == "" || s.Request.RepositoryID <= 0 || s.Request.InstallationID <= 0 || !candidateSourceCommit(s.Source.TreeSHA) || !candidateSourceCommit(s.Source.ProofSHA) || len(s.Source.Checks) == 0 {
		return errors.New("release selection lacks its verified source identity")
	}
	for _, check := range s.Source.Checks {
		if check.Context == "" || check.AppID <= 0 || check.SHA != s.Source.ProofSHA || check.Status != "completed" || check.Conclusion != "success" {
			return errors.New("release selection lacks required source verification")
		}
	}
	switch r.Phase {
	case "prepared", "tagging", "building", "built", "ready", "publishing", "published", "blocked", "uncertain":
	default:
		return errors.New("invalid release journal phase")
	}
	if r.RunAttempted && !r.TagConfirmed || r.Run != nil && !r.RunAttempted || r.CandidateID != "" && (r.Run == nil || r.Run.Status != "completed" || r.Run.Conclusion != "success" || r.Run.CancelRequested || r.Run.WorkRunID == "") || r.PublicationStarted && (r.CandidateID == "" || r.ApprovalID == "") {
		return errors.New("release journal lost a prerequisite effect fence")
	}
	if _, err := workflowhost.PrepareSnapshot(r.Selection.Plan, buildPlanContract, workflowhost.Options{}); err != nil {
		return err
	}
	if _, err := workflowhost.PrepareSnapshot(r.Selection.Workflow, buildContract, workflowhost.Options{Operations: buildCaptureOperations(false)}); err != nil {
		return err
	}
	if _, err := workflowhost.PrepareSnapshot(r.Selection.Publication, buildPublicationContract, workflowhost.Options{Operations: buildCaptureOperations(true)}); err != nil {
		return err
	}
	if r.Run != nil && !matchingBuildRun(r.Selection, *r.Run) {
		return errors.New("release build journal run binding is invalid")
	}
	if r.CandidateID != "" && (!strings.HasPrefix(r.CandidateID, "sha256:") || !candidateArtifactDigest(strings.TrimPrefix(r.CandidateID, "sha256:"))) {
		return errors.New("invalid release build candidate")
	}
	return nil
}

// Provider observations change as the build progresses; they must not create a
// new intent for the same selected release after a lost reply. Profile/grant
// changes cannot reset its fences: the original captured selection still gates
// every effect, and a new preparation recovers that same immutable history.
func buildRequestID(s releaseBuildSelection) (string, error) {
	return profileDigest(struct{ Owner, Pipeline, Repository, SHA, Tag string }{s.Owner, s.Request.PipelineID, s.Request.Repository, s.Request.ExpectedSHA, s.Tag})
}

func matchingBuildRun(s releaseBuildSelection, r pl.ReleaseBuildRun) bool {
	return r.ID != "" && r.OwnerUserID == s.Owner && r.PipelineID == s.Request.PipelineID && r.Repository == s.Request.Repository && r.SHA == s.Request.ExpectedSHA && r.Tag == s.Tag && r.Mode == "full" && r.Event == "release" && r.Attempt > 0
}
