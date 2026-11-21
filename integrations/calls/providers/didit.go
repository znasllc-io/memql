package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

const diditBase = "https://verification.didit.me/v3"

type DiditConfig struct {
	Mode            Mode
	APIKey          string
	WebhookSecret   string
	WorkflowID      string
	WorkflowVersion int
	Client          *http.Client
}
type Didit struct {
	providerHTTP
	key           string
	webhookSecret string
	workflow      string
	version       int
}

func NewDidit(c DiditConfig) (*Didit, error) {
	h, err := newHTTP(c.Mode, c.Client)
	if err != nil {
		return nil, err
	}
	if h.mode == Sandbox && (c.APIKey == "" || !validIdentifier(c.WorkflowID) || c.WorkflowVersion < 1) {
		return nil, errors.New("Didit sandbox requires key and pinned workflow version")
	}
	return &Didit{providerHTTP: h, key: c.APIKey, webhookSecret: c.WebhookSecret, workflow: c.WorkflowID, version: c.WorkflowVersion}, nil
}
func (d *Didit) headers() http.Header {
	return http.Header{"X-Api-Key": {d.key}, "Content-Type": {"application/json"}, "Accept": {"application/json"}}
}

type diditFeature struct {
	Status   string `json:"status"`
	Age      *int   `json:"age"`
	Method   string `json:"verification_method"`
	Warnings []struct {
		Code string `json:"code"`
	} `json:"warnings"`
}
type diditDecision struct {
	ID              string            `json:"session_id"`
	URL             string            `json:"url"`
	SessionURL      string            `json:"session_url"`
	Status          string            `json:"status"`
	AccountID       string            `json:"vendor_data"`
	WorkflowID      string            `json:"workflow_id"`
	WorkflowVersion int               `json:"workflow_version"`
	Environment     string            `json:"environment"`
	Metadata        map[string]string `json:"metadata"`
	IDVerifications []diditFeature    `json:"id_verifications"`
	LivenessChecks  []diditFeature    `json:"liveness_checks"`
	FaceMatches     []diditFeature    `json:"face_matches"`
	// A manual override is not automatically accepted as document/liveness proof.
	StatusOverride json.RawMessage `json:"status_override"`
}

func (d *Didit) result(v diditDecision, created bool) (Verification, error) {
	if !validIdentifier(v.ID) || v.WorkflowID != d.workflow {
		return Verification{}, errors.New("Didit session does not match the configured workflow")
	}
	if v.Environment != "" && v.Environment != "sandbox" {
		return Verification{}, ErrWrongEnvironment
	}
	if v.WorkflowVersion != d.version {
		return Verification{}, errors.New("Didit workflow version is not the reviewed version")
	}
	if v.URL == "" {
		v.URL = v.SessionURL
	}
	result := Verification{ID: v.ID, URL: v.URL, Status: v.Status, AccountID: v.AccountID, OperationID: v.Metadata["operation_id"], WorkflowID: v.WorkflowID, WorkflowVersion: v.WorkflowVersion}
	// Preserve only the underage conclusion, never the DOB or numeric age.
	// An explicit final document minimum-age failure must not become a fresh
	// paid retry merely because the aggregate result is the generic Declined.
	if !created && (v.Status == "Declined" || v.Status == "Approved") {
		for _, f := range v.IDVerifications {
			if f.Method != "document" {
				continue
			}
			if f.Age != nil && *f.Age >= 0 && *f.Age < 18 {
				result.Underage = true
			}
			for _, warning := range f.Warnings {
				if warning.Code == "MINIMUM_AGE_NOT_MET" {
					result.Underage = true
				}
			}
		}
	}
	// Only document-backed evidence can grant the requested 18+ assurance. The
	// aggregate status can be manually overridden, or belong to a workflow that
	// proves identity without proving age. Missing data is not a successful check.
	if !created && v.Status == "Approved" && (len(v.StatusOverride) == 0 || string(v.StatusOverride) == "null") && approved(v.IDVerifications) && approved(v.LivenessChecks) && approved(v.FaceMatches) {
		adult := false
		for _, f := range v.IDVerifications {
			if f.Age != nil && *f.Age >= 18 && f.Method == "document" {
				adult = true
			}
			if f.Age != nil && *f.Age < 18 {
				adult = false
				break
			}
		}
		if adult && !result.Underage {
			result.Adult18 = true
			result.Method = "document_liveness_face_match"
			result.VerifiedAt = time.Now().UTC()
		}
	}
	return result, nil
}
func approved(features []diditFeature) bool {
	if len(features) == 0 {
		return false
	}
	for _, f := range features {
		if f.Status != "Approved" {
			return false
		}
		for _, w := range f.Warnings {
			if w.Code == "MINIMUM_AGE_NOT_MET" {
				return false
			}
		}
	}
	return true
}
func (d *Didit) CreateVerification(ctx context.Context, r VerificationRequest) (Verification, error) {
	if err := d.enabled(); err != nil {
		return Verification{}, err
	}
	if !validIdentifier(r.OperationID) || !validIdentifier(r.AccountID) || !validReturnURL(r.CallbackURL) {
		return Verification{}, errors.New("invalid verification request")
	}
	scenario := r.Scenario
	if scenario == "" {
		scenario = "approve"
	}
	if !validIdentifier(scenario) {
		return Verification{}, errors.New("invalid sandbox scenario")
	}
	// This is deliberately always supplied. Didit rejects sandbox_scenario on a
	// live application, so even an accidentally supplied live key cannot start a
	// billable production capture through this sandbox adapter.
	data, _ := json.Marshal(map[string]any{"workflow_id": d.workflow, "vendor_data": r.AccountID, "callback": r.CallbackURL, "callback_method": "both", "metadata": map[string]string{"operation_id": r.OperationID}, "sandbox_scenario": scenario})
	var v diditDecision
	_, err := d.request(ctx, http.MethodPost, diditBase+"/session/", bytes.NewReader(data), d.headers(), &v)
	if err != nil {
		return Verification{}, err
	}
	return d.result(v, true)
}
func (d *Didit) ReadVerification(ctx context.Context, id string) (Verification, error) {
	if !validIdentifier(id) {
		return Verification{}, errors.New("invalid verification id")
	}
	var v diditDecision
	_, err := d.request(ctx, http.MethodGet, diditBase+"/session/"+url.PathEscape(id)+"/decision/?include=events", nil, d.headers(), &v)
	if err != nil {
		return Verification{}, err
	}
	return d.result(v, false)
}
func (d *Didit) VerifyWebhook(body []byte, headers http.Header, now time.Time) (Webhook, error) {
	return VerifyDiditWebhook(body, headers, d.webhookSecret, now)
}
