// Package installation holds the native authority and effect journal for an
// installation revision. Operator-selected installed DSL owns sequencing;
// native scoped ports verify authority, artifacts, preserved resources and
// runtime health before recording preparation, application or completion.
package installation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const maxPlanBytes = 5 << 20

var (
	identifier     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	internalDigest = regexp.MustCompile(`^memql-id:[0-9a-f]{64}$`)
	artifactDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitDigest   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// preparedPlan is native-only input, never a client/DSL argument. The binding
// digests name the independently verified published candidate, full rendered
// resource diff, and immutable rollback render. Persisting these bindings does
// NOT verify them. The scoped preparer supplies actual proofs before using
// this journal; public selections never supply evidence or effect authority.
type preparedApplication struct {
	Key                    string        `json:"key"`
	Intent                 argocd.Intent `json:"intent"`
	CandidateCapture       string        `json:"candidateCapture"`
	RollbackCapture        string        `json:"rollbackCapture"`
	RenderDigest           string        `json:"renderDigest"`
	RollbackRenderDigest   string        `json:"rollbackRenderDigest"`
	CandidateSourceDigest  string        `json:"candidateSourceDigest"`
	RollbackSourceDigest   string        `json:"rollbackSourceDigest"`
	CandidateReceiptDigest string        `json:"candidateReceiptDigest"`
	RollbackReceiptDigest  string        `json:"rollbackReceiptDigest"`
}

type preparedPlan struct {
	RevisionWorkflow        *workflowhost.Snapshot `json:"revisionWorkflow,omitempty"`
	Applications            []preparedApplication  `json:"applications,omitempty"`
	RollbackWorkflow        *workflowhost.Snapshot `json:"rollbackWorkflow,omitempty"`
	CompletionWorkflow      *workflowhost.Snapshot `json:"completionWorkflow,omitempty"`
	HealthBaseline          *HealthObservation     `json:"healthBaseline,omitempty"`
	FormatVersion           int                    `json:"formatVersion"`
	InstallationID          string                 `json:"installationId"`
	RequestedBy             string                 `json:"requestedBy"`
	WorkflowDigest          string                 `json:"workflowDigest"`
	ExecutionWorkflowDigest string                 `json:"executionWorkflowDigest"`
	CandidateID             string                 `json:"candidateId"`
	CandidateApprovalID     string                 `json:"candidateApprovalId"`
	PublicationDigest       string                 `json:"publicationDigest"`
	RenderDigest            string                 `json:"renderDigest"`
	ResourceDiffDigest      string                 `json:"resourceDiffDigest"`
	RollbackRevision        string                 `json:"rollbackRevision"`
	RollbackRenderDigest    string                 `json:"rollbackRenderDigest"`
	Preparation             *preparationBinding    `json:"preparation,omitempty"`
	Intent                  argocd.Intent          `json:"intent"`
}

func (p preparedPlan) canonical() ([]byte, string, error) {
	if (p.FormatVersion != 1 && p.FormatVersion != 2) || !identifier.MatchString(p.InstallationID) || !identifier.MatchString(p.RequestedBy) ||
		!identifier.MatchString(p.CandidateApprovalID) || !artifactDigest.MatchString(p.CandidateID) || !artifactDigest.MatchString(p.PublicationDigest) || !commitDigest.MatchString(p.RollbackRevision) {
		return nil, "", errors.New("installation plan has invalid identity or candidate bindings")
	}
	for _, digest := range []string{p.WorkflowDigest, p.ExecutionWorkflowDigest, p.RenderDigest, p.ResourceDiffDigest, p.RollbackRenderDigest} {
		if !internalDigest.MatchString(digest) {
			return nil, "", errors.New("installation plan requires exact native evidence bindings")
		}
	}
	if p.RevisionWorkflow != nil && (p.RevisionWorkflow.Contract != revisionContract || p.RevisionWorkflow.CheckArgs(nil) != nil) {
		return nil, "", errors.New("installation revision recipe is invalid")
	}
	if p.RollbackWorkflow != nil && (p.RollbackWorkflow.Contract != rollbackContract || p.RollbackWorkflow.CheckArgs(nil) != nil) {
		return nil, "", errors.New("installation rollback recipe is invalid")
	}
	if p.CompletionWorkflow != nil && (p.CompletionWorkflow.Contract != completionContract || p.CompletionWorkflow.CheckArgs(nil) != nil) {
		return nil, "", errors.New("installation completion recipe is invalid")
	}
	if p.HealthBaseline != nil {
		if err := p.HealthBaseline.validate(); err != nil {
			return nil, "", err
		}
	}
	if p.Preparation != nil {
		if err := p.Preparation.validate(); err != nil {
			return nil, "", err
		}
	}
	if _, err := p.Intent.Digest(); err != nil {
		return nil, "", err
	}
	var spec struct {
		Source struct {
			TargetRevision string `json:"targetRevision"`
		} `json:"source"`
	}
	if json.Unmarshal(p.Intent.BeforeSpec, &spec) != nil || spec.Source.TargetRevision != p.RollbackRevision || p.Intent.Revision == p.RollbackRevision {
		return nil, "", errors.New("installation rollback must bind the immutable starting revision")
	}
	if p.FormatVersion == 1 && len(p.Applications) != 0 {
		return nil, "", errors.New("legacy plan cannot contain application group members")
	}
	if p.FormatVersion == 2 {
		if len(p.Applications) == 0 || len(p.Applications) > 8 || !sameIntent(p.Applications[0].Intent, p.Intent) {
			return nil, "", errors.New("group plan requires its exact complete application set")
		}
		targets := map[argocd.Target]bool{}
		last := ""
		for _, app := range p.Applications {
			if !identifier.MatchString(app.Key) || app.Key <= last || targets[app.Intent.Target] || app.Intent.Revision != p.Intent.Revision || app.Intent.Prune != p.Intent.Prune || beforeRevision(app.Intent.BeforeSpec) != p.RollbackRevision || app.CandidateCapture != applicationCaptureKey(app.Key, "candidate") || app.RollbackCapture != applicationCaptureKey(app.Key, "rollback") {
				return nil, "", errors.New("group plan member scope differs")
			}
			if _, err := app.Intent.Digest(); err != nil {
				return nil, "", err
			}
			for _, digest := range []string{app.RenderDigest, app.RollbackRenderDigest, app.CandidateSourceDigest, app.RollbackSourceDigest, app.CandidateReceiptDigest, app.RollbackReceiptDigest} {
				if !internalDigest.MatchString(digest) {
					return nil, "", errors.New("group member lacks exact source and render proofs")
				}
			}
			last = app.Key
			targets[app.Intent.Target] = true
		}
	}
	if p.RollbackWorkflow != nil && (p.RollbackWorkflow.Contract != rollbackContract || p.RollbackWorkflow.CheckArgs(nil) != nil) {
		return nil, "", errors.New("installation rollback recipe is invalid")
	}
	body, err := json.Marshal(p)
	if err != nil || len(body) > maxPlanBytes {
		return nil, "", errors.New("installation plan exceeds its encoding bound")
	}
	body, err = canonicalJSON(body)
	if err != nil {
		return nil, "", err
	}
	return body, "memql-id:" + string(id.NewUntracked().FromString(fmt.Sprintf("installation-revision-plan-v%d:", p.FormatVersion)+string(body))), nil
}

func decodePlan(body []byte) (preparedPlan, string, error) {
	var plan preparedPlan
	if len(body) > maxPlanBytes {
		return plan, "", errors.New("installation plan exceeds its encoding bound")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&plan); err != nil {
		return plan, "", err
	}
	canonical, key, err := plan.canonical()
	if err != nil {
		return plan, "", err
	}
	stored, err := canonicalJSON(body)
	if err != nil || !bytes.Equal(stored, canonical) {
		return plan, "", errors.New("stored installation plan has noncanonical fields")
	}
	// In particular, do not allow unknown fields inside the protocol intent to
	// disappear during typed decoding. Its nested spec is validated by Argo.
	intent, err := json.Marshal(plan.Intent)
	if err != nil {
		return plan, "", err
	}
	plan.Intent, err = argocd.DecodeIntent(intent)
	return plan, key, err
}

// JSONB rewrites object order and spacing. Normalize both typed and stored
// forms, preserving integer values, before comparing or naming a plan.
func canonicalJSON(body []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing installation data")
	}
	return json.Marshal(value)
}
