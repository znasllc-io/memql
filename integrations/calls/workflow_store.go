package calls

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const workflowConcept = "v1:calls:workflowState"

type workflowOperation struct {
	Effect             *callrules.WorkflowEffect `json:"effect,omitempty"`
	Admission          string                    `json:"admission,omitempty"`
	TargetID           string
	Key, Digest, Actor string
	Revision           int64
}
type workflowState struct {
	Mute              *workflowMuteIntent            `json:"mute,omitempty"`
	Binding           *callrules.WorkflowBinding     `json:"binding,omitempty"`
	BookingDigest     string                         `json:"bookingDigest,omitempty"`
	ID, SiteID, KeyID string                         `json:"-"`
	Version           time.Time                      `json:"-"`
	Kind              string                         `json:"kind"`
	Definitions       []callrules.WorkflowDefinition `json:"definitions,omitempty"`
	Run               *callrules.WorkflowRun         `json:"run,omitempty"`
	Operations        []workflowOperation            `json:"operations"`
}

func workflowStateID(site, kind, key string) string {
	return callrules.HistoryDigest(nil, "workflow-"+kind, site, key)[:32]
}
func (s *Store) workflowState(ctx context.Context, owner, id string) (*workflowState, error) {
	call, err := langparser.RenderCall("callsWorkflowStateById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the workflow state", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	row := rows[0]
	var v workflowState
	data := rowString(row, "stateData")
	if len(data) > 1<<20 || json.Unmarshal([]byte(data), &v) != nil || len(v.Definitions) > 96 || len(v.Operations) > 1000 || (v.Kind != "library" && v.Kind != "run" && v.Kind != "booking") {
		return nil, accountRefusal("workflow_state_unavailable")
	}
	v.ID = memql.BareShortId(rowString(row, "id"))
	v.SiteID = memql.BareShortId(rowString(row, "siteId"))
	v.KeyID = memql.BareShortId(rowString(row, "keyId"))
	v.Version = rowTime(row, "createdAt")
	// These additive projection fields are absent on pre-release local fixtures.
	// All native writes populate them; an explicit but inconsistent projection
	// fails closed rather than hiding a live run from reconciliation.
	if kind := rowString(row, "kind"); kind != "" && (kind != v.Kind || rowBool(row, "active") != workflowActive(&v)) {
		return nil, accountRefusal("workflow_state_unavailable")
	}
	if v.ID != id || v.KeyID == "" || v.SiteID == "" {
		return nil, accountRefusal("workflow_state_unavailable")
	}
	if v.Kind == "run" && (v.Run == nil || v.Run.RoomID == "" || len(callrules.ValidateWorkflow(v.Run.Definition)) != 0 || len(v.Run.Events) > 256 || len(v.Run.History) > 256) {
		return nil, accountRefusal("workflow_state_unavailable")
	}

	for _, d := range v.Definitions {
		if len(callrules.ValidateWorkflow(d)) != 0 || (d.Status != "draft" && d.Status != "published") || d.Version < 1 || d.Revision < 1 {
			return nil, accountRefusal("workflow_state_unavailable")
		}
	}
	if v.Kind == "run" {
		r := v.Run
		_, nodeExists := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
		if !callrules.ValidWorkflowID(r.ID) || r.Definition.Status != "published" || r.DefinitionID != r.Definition.ID || r.DefinitionVersion != r.Definition.Version || r.Revision < 1 || r.ControlEpoch < 1 || r.VisitNumber < 1 || !nodeExists || r.ElapsedSeconds < 0 || r.DurationSeconds < 0 || r.DurationSeconds > 14400 || len(r.Bindings) > 16 || len(r.AcknowledgedSeatIDs) > 100 || !slices.Contains([]string{"running", "paused", "blocked", "completed", "cancelled"}, r.Status) || (r.Hold != "" && r.Hold != "human") || (r.Status == "running" && r.AnchorAt.IsZero()) {
			return nil, accountRefusal("workflow_state_unavailable")
		}
	}
	if !workflowValidMute(&v) {
		return nil, accountRefusal("workflow_state_unavailable")
	}
	if v.Kind == "booking" && (v.Binding == nil || !callrules.ValidWorkflowID(v.Binding.BookingID) || !callrules.ValidWorkflowID(v.Binding.SkillID) || v.Binding.Revision < 1 || v.Binding.Version < 1 || len(v.Binding.Bindings) > 16 || len(v.BookingDigest) != 64) {
		return nil, accountRefusal("workflow_state_unavailable")
	}
	return &v, nil
}
func (s *Store) saveWorkflow(ctx context.Context, owner string, v *workflowState, now time.Time) error {
	workflowInvalidateMute(v)
	data, err := json.Marshal(v)
	if err != nil || len(data) > 1<<20 {
		return accountRefusal("workflow_limit")
	}
	version := memql.VersionTimeAfter(v.Version, now)
	call, err := langparser.RenderCall("saveCallsWorkflowState", map[string]any{"stateId": v.ID, "versionTime": versionStamp(version), "siteId": v.SiteID, "keyId": v.KeyID, "stateData": string(data), "kind": v.Kind, "active": workflowActive(v)})
	fence := absent(workflowConcept, v.ID)
	if !v.Version.IsZero() {
		fence = at(workflowConcept, v.ID, v.Version)
	}
	err = s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, fence, "the workflow state", call, err)
	if err == nil {
		v.Version = version
	}
	return err
}

// Every durable write updates this projection in the same fenced version as its
// journal. Historical libraries, bookings and finished runs cannot fill the
// active reconciliation scan and starve current calls.
func workflowActive(v *workflowState) bool {
	return v.Kind == "run" && v.Run != nil && !workflowTerminal(v.Run)
}
