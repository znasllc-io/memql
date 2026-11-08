package pipelinerun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type SecurityGoalRequest struct {
	Key, OwnerUserID, Statement string
	AccountIDs                  []string
	Input                       map[string]any
	// Check is bound to the claimed run and current consent; it is never a
	// caller-supplied boolean or serializable grant.
	Check func(context.Context) error
}

type securityReportScope struct {
	run           Run
	pipeline      Pipeline
	artifact      pipelinesteps.StoredFileReceipt
	findings      []pipelines.SecurityFinding
	byFingerprint map[string]pipelines.SecurityFinding
	goals         map[string]string
	check         func(context.Context) error
}

// readSecurityReport consumes the exact immutable artifact already retained by
// a successful scanner. It needs source read authority, never GitHub's SARIF
// publishing API. Scanner failures, incomplete receipts and changed bytes are
// errors; none can become an empty clean scan.
func (i *Integration) readSecurityReport(ctx context.Context, runID, stepKey, filePath string, fence func(context.Context) error) (*securityReportScope, error) {
	d := i.snapshot()
	if d.Store == nil || d.GitHub == nil || d.SARIF == nil || d.SARIF.Engine == nil || d.SARIF.Library == nil {
		return nil, errors.New("native security report storage is unavailable")
	}
	owner, err := personFrom(ctx)
	if err != nil {
		return nil, err
	}
	owner = memql.BareShortId(owner)
	r, p, err := sarifSource(ctx, d, owner, runID)
	if err != nil {
		return nil, err
	}
	check := func(c context.Context) error {
		if err := fence(c); err != nil {
			return err
		}
		if !d.securitySettings(c).Enabled {
			return errors.New("Weekly security scans are disabled in Pipelines settings")
		}
		current, err := d.Store.PipelineByID(memql.ContextWithFreshRead(c), p.ID)
		if err != nil {
			return err
		}
		if current == nil || !current.Active() || current.OwnerUserID != p.OwnerUserID || current.Repository != p.Repository || current.AccountID != p.AccountID ||
			(r.Event != pipelines.EventSchedule && r.Event != pipelines.EventManual) || r.Qualification != nil {
			return errors.New("security report source is no longer an active scheduled or manual pipeline")
		}
		_, err = sarifGrant(c, d, owner, r, p)
		return err
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	proof, err := readProducerEvidence(ctx, d.SARIF.Engine, owner, r, stepKey)
	if err != nil {
		return nil, err
	}
	scope := pipelinesteps.RunFileReceiptScope{OwnerUserID: owner, WorkRunID: r.WorkRunID, StepKey: stepKey, Attempt: proof.Attempt}
	rows, err := d.SARIF.Library.ReadRunFileReceipts(auth.ContextWithInternalOrigin(ctx), scope, proof.ArtifactIntentIDs)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(proof.ArtifactIntentIDs) {
		return nil, errors.New("security scanner has incomplete artifact receipts")
	}
	seen := map[string]bool{}
	var artifact pipelinesteps.StoredFileReceipt
	for _, row := range rows {
		if row.OwnerUserID != owner || row.WorkRunID != scope.WorkRunID || row.StepKey != stepKey || row.Attempt != scope.Attempt ||
			!slices.Contains(proof.ArtifactIntentIDs, row.IntentID) || seen[row.IntentID] {
			return nil, errors.New("security artifact receipt is outside its scanner scope")
		}
		seen[row.IntentID] = true
		if row.Path == filePath {
			if artifact.IntentID != "" {
				return nil, errors.New("security artifact path has ambiguous receipts")
			}
			artifact = row
		}
	}
	digest, decodeErr := hex.DecodeString(artifact.SHA256)
	if artifact.IntentID == "" || artifact.ETag == "" || artifact.Size <= 0 || artifact.Size > pipelines.MaxSecurityReportBytes || decodeErr != nil || len(digest) != 32 {
		return nil, errors.New("security artifact has no bounded immutable byte receipt")
	}
	// The owning pipeline is still active while its report step runs, so the
	// existing producer-retention fence protects these bytes without a new pin.
	body, err := readRetainedArtifactBytes(ctx, d.SARIF.Library, scope, artifact)
	if err != nil {
		return nil, err
	}
	findings, err := pipelines.ParseSecurityFindings(body)
	if err != nil {
		return nil, err
	}
	bound := &securityReportScope{run: r, pipeline: p, artifact: artifact, findings: findings,
		byFingerprint: map[string]pipelines.SecurityFinding{}, goals: map[string]string{}, check: check}
	for _, finding := range findings {
		bound.byFingerprint[finding.Fingerprint] = finding
	}
	return bound, nil
}

func (i *Integration) openSecurityFinding(ctx context.Context, report *securityReportScope, fingerprint, statement string) (string, error) {
	finding, ok := report.byFingerprint[fingerprint]
	if !ok || statement == "" || len(statement) > 16000 {
		return "", errors.New("security goal must name one verified finding and a bounded statement")
	}
	if err := report.check(ctx); err != nil {
		return "", err
	}
	d := i.snapshot()
	if d.OpenSecurityGoal == nil {
		return "", errors.New("security remediation goal service is unavailable")
	}
	identity := sha256.Sum256([]byte(report.pipeline.ID + "\x00" + report.pipeline.Repository + "\x00" + fingerprint))
	input := map[string]any{"pipelineId": report.pipeline.ID, "repository": report.run.Repository, "sha": report.run.SHA, "runId": report.run.ID,
		"artifactPath": report.artifact.Path, "artifactSHA256": report.artifact.SHA256, "finding": finding}
	accounts := []string{}
	if report.pipeline.AccountID != "" {
		accounts = append(accounts, report.pipeline.AccountID)
	}
	goal, err := d.OpenSecurityGoal(ctx, SecurityGoalRequest{Key: hex.EncodeToString(identity[:]), OwnerUserID: report.run.OwnerUserID,
		Statement: statement, AccountIDs: accounts, Input: input, Check: report.check})
	if err != nil {
		return "", err
	}
	if goal == "" {
		return "", errors.New("security goal service returned no durable identity")
	}
	report.goals[fingerprint] = goal
	return goal, nil
}

// The installed DSL selects the goal statement and ordering. Each call-local
// operation binds to this report's verified scope; no supplied raw report,
// owner, repository or arbitrary finding can reach the work admission seam.
func (dr *runDriver) securityReport(ctx context.Context, t *stepTrack, fence func() error) (map[string]any, error) {
	var report *securityReportScope
	_, err := dr.runPinnedWorkflowWithOptions(ctx, "pipelineSecurityRemediation", nil, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"pipelineSecurityRead": func(c context.Context, args map[string]any) (any, error) {
			if len(args) != 0 {
				return nil, errors.New("security read takes only its compiled artifact")
			}
			if report == nil {
				var err error
				report, err = dr.i.readSecurityReport(c, dr.runID, t.step.SARIF.Step, t.step.SARIF.Path, func(context.Context) error { return fence() })
				if err != nil {
					return nil, err
				}
			}
			// DSL values are ordinary maps, never Go structs.
			body, err := json.Marshal(report.findings)
			if err != nil {
				return nil, err
			}
			var findings []any
			if err := json.Unmarshal(body, &findings); err != nil {
				return nil, err
			}
			return map[string]any{"findings": findings, "repository": report.run.Repository}, nil
		},
		"pipelineSecurityOpenGoal": func(c context.Context, args map[string]any) (any, error) {
			if report == nil || len(args) != 2 {
				return nil, errors.New("security goal requires a verified report scope")
			}
			goal, err := dr.i.openSecurityFinding(c, report, stringArg(args, "fingerprint"), stringArg(args, "statement"))
			return map[string]any{"goalId": goal}, err
		},
	}})
	if err != nil {
		return nil, err
	}
	if report == nil || len(report.goals) != len(report.findings) {
		return nil, fmt.Errorf("security workflow did not account for every verified finding")
	}
	goalIDs := make([]string, 0, len(report.goals))
	for _, finding := range report.findings {
		goalIDs = append(goalIDs, report.goals[finding.Fingerprint])
	}
	return map[string]any{"status": string(pipelines.OutcomeSucceeded), "findings": len(report.findings), "goalIds": goalIDs, "sha256": report.artifact.SHA256, "destination": "goals"}, nil
}
