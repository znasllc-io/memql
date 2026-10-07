package installation

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/znasllc-io/memql/core/id"
)

// Native receiver configuration supplies these values. The UI may select a
// candidate/request identity, never submit this scope or a claimed digest.
// RequestID is stable across retries. Read an existing reservation before
// choosing its initial capture start time; a retry cannot rewrite that scope.
type preparationScope struct {
	FormatVersion       int                          `json:"formatVersion"`
	InstallationID      string                       `json:"installationId"`
	RequestID           string                       `json:"requestId"`
	RequestedBy         string                       `json:"requestedBy"`
	WorkflowDigest      string                       `json:"workflowDigest"`
	ConfigurationDigest string                       `json:"configurationDigest"`
	CandidateID         string                       `json:"candidateId"`
	PublicationDigest   string                       `json:"publicationDigest"`
	Captures            map[string]sourceCaptureSpec `json:"captures"`
}

func preparationSourceRun(installation, request, actor string) string {
	return string(id.NewUntracked().FromString("installation-preparation-source-run-v1:" + installation + ":" + request + ":" + actor))
}

func (s preparationScope) canonical() ([]byte, string, error) {
	if s.FormatVersion != 1 || !identifier.MatchString(s.InstallationID) || !identifier.MatchString(s.RequestID) ||
		!identifier.MatchString(s.RequestedBy) || !internalDigest.MatchString(s.WorkflowDigest) ||
		!internalDigest.MatchString(s.ConfigurationDigest) || !artifactDigest.MatchString(s.CandidateID) || !artifactDigest.MatchString(s.PublicationDigest) || len(s.Captures) != 2 {
		return nil, "", errors.New("installation preparation requires complete native identity and configuration bindings")
	}
	run := preparationSourceRun(s.InstallationID, s.RequestID, s.RequestedBy)
	for _, role := range []string{"candidate", "rollback"} {
		spec, present := s.Captures[role]
		if !present || spec.OwnerUserID != s.RequestedBy || spec.RunID != run || spec.WorkRunID != run || spec.Attempt != 1 || spec.StepKey != "source-"+role {
			return nil, "", errors.New("source capture does not belong to this preparation")
		}
		if _, err := newSourceCapture(spec); err != nil {
			return nil, "", err
		}
	}
	a, b := s.Captures["candidate"], s.Captures["rollback"]
	if err := sameRenderScope(b.Render, a.Render); err != nil {
		return nil, "", err
	}
	if a.Repository != b.Repository || a.RunStartedAt != b.RunStartedAt || a.CollectorImage != b.CollectorImage ||
		a.Platform != b.Platform || a.CloneInstallationID != b.CloneInstallationID || a.ImagePullSecret != b.ImagePullSecret {
		return nil, "", errors.New("candidate and rollback acquisition configuration differs")
	}
	body, err := json.Marshal(s)
	if err != nil || len(body) > 128<<10 {
		return nil, "", errors.New("preparation scope exceeds its encoding bound")
	}
	body, err = canonicalJSON(body)
	if err != nil {
		return nil, "", err
	}
	return body, "memql-id:" + string(id.NewUntracked().FromString("installation-preparation-v1:"+string(body))), nil
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
