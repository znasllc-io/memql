package pipelinerun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// ReviewSource is a current native authority observation. No public argument,
// repository manifest or model result supplies any of these fields.
type ReviewSource struct {
	OwnerUserID, PipelineID, SourceID, CredentialID, Repository, Branch string
	InstallationID, RepositoryID, AppID                                 int64
}
type ReviewMember struct {
	Kind, RunID, RequestID, ReviewDigest, ExternalID, DiffDigest string
	CheckRunID, AppID                                            int64
}
type IndependentReviewInput struct {
	Kind                      string
	Source                    ReviewSource
	PolicyVersion, PromptName string
	Prompt                    map[string]any
	PullRequest               *githubapp.PullRequestObservation
	Diff                      *githubapp.ReviewDiff
	Group                     *githubapp.MergeGroupEvidence
	Members                   []ReviewMember
}
type IndependentReviewVerdict struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"`
}
type IndependentReviewReceipt struct {
	CheckName, HeadSHA, ExternalID string
	CheckRunID, AppID              int64
}
type IndependentReviewRecord struct {
	Version            int
	RunID, InputDigest string
	Input              IndependentReviewInput
	ModelAttempted     bool
	Verdict            *IndependentReviewVerdict
	ReportAttempted    bool
	Receipt            *IndependentReviewReceipt
}

// IndependentReviewDependencies can only be installed by native wiring. The
// pipeline owns persistence/lease fencing; the adapter owns provider/source and
// prompt freshness. Report(observeOnly=true) must never create a provider check.
type IndependentReviewDependencies struct {
	Capture  func(context.Context, Pipeline, Run, string, string, func(context.Context) error) (IndependentReviewInput, error)
	Validate func(context.Context, Pipeline, Run, IndependentReviewInput, func(context.Context) error) error
	Review   func(context.Context, IndependentReviewInput) (IndependentReviewVerdict, error)
	Report   func(context.Context, IndependentReviewInput, string, bool, func(context.Context) error) (IndependentReviewReceipt, error)
}

func reviewDigest(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (r IndependentReviewRecord) ExternalID() string {
	return "memql-review:" + reviewDigest(struct {
		Domain, RunID, InputDigest string
		Verdict                    *IndependentReviewVerdict
	}{"pipeline-independent-review.v1", r.RunID, r.InputDigest, r.Verdict})
}
func readIndependentReview(value map[string]any) (IndependentReviewRecord, error) {
	var r IndependentReviewRecord
	if err := decodeSelectionObject(value, &r); err != nil || r.Version != 1 || r.RunID == "" || r.InputDigest == "" || r.InputDigest != reviewDigest(r.Input) {
		return r, errors.New("independent review evidence is missing or changed")
	}
	if r.Verdict != nil && !plainSelectionText(r.Verdict.Reason, 4000) {
		return r, errors.New("independent review reason is malformed")
	}
	if r.Input.Kind == "ordinary" && r.Verdict != nil && !r.ModelAttempted {
		return r, errors.New("ordinary review verdict has no durable model attempt")
	}
	if r.ReportAttempted && (r.Verdict == nil || !r.Verdict.Approved) {
		return r, errors.New("review reporting has no accepted verdict")
	}
	if r.Receipt != nil && (!r.ReportAttempted || !matchingIndependentReceipt(r, *r.Receipt)) {
		return r, errors.New("review receipt does not match its durable intent")
	}
	return r, nil
}
func matchingIndependentReceipt(r IndependentReviewRecord, got IndependentReviewReceipt) bool {
	head := ""
	if r.Input.PullRequest != nil {
		head = r.Input.PullRequest.HeadSHA
	}
	if r.Input.Group != nil {
		head = r.Input.Group.HeadSHA
	}
	external := r.ExternalID()
	if r.Input.Kind == "native" && len(r.Input.Members) == 1 {
		external = r.Input.Members[0].ExternalID
	}
	return got.CheckName == "MemQL / review" && got.HeadSHA == head && got.ExternalID == external && got.CheckRunID > 0 && got.AppID == r.Input.Source.AppID
}
