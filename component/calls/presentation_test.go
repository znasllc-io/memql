package calls

import "testing"

func TestPresentationRequestClosedAndVersioned(t *testing.T) {
	p := &PresentationRequest{VisitID: "visit", ExpectedRevision: 0, PresenterSeatID: "seat", Audience: &PublicationPermissions{}}
	r := WorkflowRequest{Action: "presentation_set", Presentation: p, RunID: "run", ExpectedRevision: 1, ExpectedEpoch: 1, IdempotencyKey: "set-one"}
	if !ValidPresentationRequest(r) {
		t.Fatal("complete bounded request rejected")
	}
	bad := r
	bad.Observation = &ObservationRequest{}
	if ValidPresentationRequest(bad) {
		t.Fatal("mixed observation accepted")
	}
	bad = r
	bad.Question = &QuestionRequest{}
	if ValidPresentationRequest(bad) {
		t.Fatal("mixed question accepted")
	}
	bad = r
	bad.ExpectedEpoch = 0
	if ValidPresentationRequest(bad) {
		t.Fatal("missing control epoch accepted")
	}
	copy := *p
	copy.SpeakerSeatID = "speaker"
	copy.SpeakerSeconds = 301
	bad = r
	bad.Presentation = &copy
	if ValidPresentationRequest(bad) {
		t.Fatal("unbounded speaker accepted")
	}
	copy.SpeakerSeconds = 300
	if !ValidPresentationRequest(bad) {
		t.Fatal("bounded speaker rejected")
	}
	if !ValidPresentationRequest(WorkflowRequest{Action: "presentation_status"}) {
		t.Fatal("status rejected")
	}
	if ValidPresentationRequest(WorkflowRequest{Action: "presentation_status", Presentation: p}) {
		t.Fatal("status accepted mutation fields")
	}
	r.Action = "presentation_clear"
	r.Presentation = &PresentationRequest{VisitID: "visit", ExpectedRevision: 1}
	if !ValidPresentationRequest(r) {
		t.Fatal("clear rejected")
	}
}
