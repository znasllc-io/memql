package installation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// Native receiver configuration supplies these values. The UI may select a
// candidate/request identity, never submit this scope or a claimed digest.
// RequestID is stable across retries. Read an existing reservation before
// choosing its initial capture start time; a retry cannot rewrite that scope.
type preparationApplication struct {
	Key    string        `json:"key"`
	Intent argocd.Intent `json:"intent"`
}

type preparationScope struct {
	Applications                 []preparationApplication     `json:"applications,omitempty"`
	FormatVersion                int                          `json:"formatVersion"`
	InstallationID               string                       `json:"installationId"`
	RequestID                    string                       `json:"requestId"`
	RequestedBy                  string                       `json:"requestedBy"`
	WorkflowDigest               string                       `json:"workflowDigest"`
	ExecutionWorkflowDigest      string                       `json:"executionWorkflowDigest"`
	ConfigurationInvariantDigest string                       `json:"configurationInvariantDigest"`
	ConfigurationDigest          string                       `json:"configurationDigest"`
	CandidateID                  string                       `json:"candidateId"`
	PublicationDigest            string                       `json:"publicationDigest"`
	Intent                       argocd.Intent                `json:"intent"`
	Captures                     map[string]sourceCaptureSpec `json:"captures"`
}

func preparationSourceRun(installation, request, actor string) string {
	return string(id.NewUntracked().FromString("installation-preparation-source-run-v1:" + installation + ":" + request + ":" + actor))
}

func (s preparationScope) canonical() ([]byte, string, error) {
	if (s.FormatVersion != 1 && s.FormatVersion != 2) || !identifier.MatchString(s.InstallationID) || !identifier.MatchString(s.RequestID) ||
		!identifier.MatchString(s.RequestedBy) || !internalDigest.MatchString(s.WorkflowDigest) ||
		!internalDigest.MatchString(s.ExecutionWorkflowDigest) || !internalDigest.MatchString(s.ConfigurationDigest) || !internalDigest.MatchString(s.ConfigurationInvariantDigest) || !artifactDigest.MatchString(s.CandidateID) || !artifactDigest.MatchString(s.PublicationDigest) || len(s.Captures) != 2*len(s.applicationMembers()) {
		return nil, "", errors.New("installation preparation requires complete native identity and configuration bindings")
	}
	run := preparationSourceRun(s.InstallationID, s.RequestID, s.RequestedBy)
	last := ""
	targets := map[argocd.Target]bool{}
	if s.FormatVersion == 1 && len(s.Applications) != 0 {
		return nil, "", errors.New("legacy preparation cannot contain group members")
	}
	if s.FormatVersion == 2 && (len(s.Applications) == 0 || len(s.Applications) > 8 || !sameIntent(s.Applications[0].Intent, s.Intent)) {
		return nil, "", errors.New("preparation group has no exact complete members")
	}
	for _, app := range s.applicationMembers() {
		if s.FormatVersion == 2 && (!identifier.MatchString(app.Key) || app.Key <= last || targets[app.Intent.Target] || app.Intent.Revision != s.Intent.Revision || app.Intent.Prune != s.Intent.Prune || beforeRevision(app.Intent.BeforeSpec) != beforeRevision(s.Intent.BeforeSpec)) {
			return nil, "", errors.New("preparation group members differ from shared selection")
		}
		last = app.Key
		targets[app.Intent.Target] = true
		for _, role := range []string{"candidate", "rollback"} {
			key := applicationCaptureKey(app.Key, role)
			spec, present := s.Captures[key]
			if !present || spec.OwnerUserID != s.RequestedBy || spec.RunID != run || spec.WorkRunID != run || spec.Attempt != 1 || spec.StepKey != "source-"+key {
				return nil, "", errors.New("source capture does not belong to this preparation")
			}
			if _, err := newSourceCapture(spec); err != nil {
				return nil, "", err
			}
		}
		a, b := s.Captures[applicationCaptureKey(app.Key, "candidate")], s.Captures[applicationCaptureKey(app.Key, "rollback")]
		if err := sameRenderScope(b.Render, a.Render); err != nil {
			return nil, "", err
		}
		if a.Repository != b.Repository || a.RunStartedAt != b.RunStartedAt || a.CollectorImage != b.CollectorImage || a.Platform != b.Platform || a.CloneInstallationID != b.CloneInstallationID || a.ImagePullSecret != b.ImagePullSecret {
			return nil, "", errors.New("candidate and rollback acquisition configuration differs")
		}
		memberScope := preparationScope{Intent: app.Intent, Captures: map[string]sourceCaptureSpec{"candidate": a, "rollback": b}}
		if err := promotionIntentMatches(memberScope, preparedPlan{Intent: app.Intent}); err != nil {
			return nil, "", err
		}
	}

	if _, err := s.Intent.Digest(); err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(s)
	if err != nil || len(body) > 128<<10 {
		return nil, "", errors.New("preparation scope exceeds its encoding bound")
	}
	body, err = canonicalJSON(body)
	if err != nil {
		return nil, "", err
	}
	return body, "memql-id:" + string(id.NewUntracked().FromString(fmt.Sprintf("installation-preparation-v%d:", s.FormatVersion)+string(body))), nil
}

func decodePreparationScope(body []byte) (preparationScope, string, error) {
	var scope preparationScope
	if len(body) > 128<<10 {
		return scope, "", errors.New("preparation scope exceeds its encoding bound")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&scope) != nil {
		return scope, "", errors.New("stored preparation scope is malformed")
	}
	expected, key, err := scope.canonical()
	if err != nil {
		return scope, "", err
	}
	actual, err := canonicalJSON(body)
	if err != nil || !bytes.Equal(actual, expected) {
		return scope, "", errors.New("stored preparation scope contains noncanonical fields")
	}
	return scope, key, nil
}

func applicationCaptureKey(application, role string) string {
	if application == "" {
		return role
	}
	return application + "-" + role
}
func (s preparationScope) applicationMembers() []preparationApplication {
	if len(s.Applications) != 0 {
		return append([]preparationApplication(nil), s.Applications...)
	}
	return []preparationApplication{{Intent: s.Intent}}
}
func (s preparationScope) sourceKey(args map[string]any) (string, error) {
	role, ok := args["role"].(string)
	application, _ := args["application"].(string)
	if !ok || (role != "candidate" && role != "rollback") || len(args) > 2 {
		return "", errors.New("source operation requires a declared application and role")
	}
	for key := range args {
		if key != "role" && key != "application" {
			return "", errors.New("source operation accepts no caller evidence")
		}
	}
	for _, app := range s.applicationMembers() {
		if app.Key == application {
			return applicationCaptureKey(application, role), nil
		}
	}
	return "", errors.New("application is outside the reserved source scope")
}
