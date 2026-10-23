package access

import (
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// ===========================================================================
// A REFUSAL ARRIVES AS A SUCCESSFUL RESPONSE
// ===========================================================================
// These are the branches the docs are most emphatic about, and until
// summaryFromResponse was split out of Fetch every one of them sat behind a
// live gRPC dial -- unreachable from the suite, and deletable without turning
// anything red. Deleting the refusal check makes `memql access` render an
// empty record ("you hold nothing") for a caller the cluster actually refused,
// which is the confusion the whole package exists to prevent.

func refusal(code, message string) *memqlv1.MemqlServerMessage {
	return &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_QueryError{
			QueryError: &memqlv1.QueryErrorMsg{
				Error: &memqlv1.QueryError{Code: code, Message: message},
			},
		},
	}
}

func TestSummaryFromResponseSurfacesARefusalRatherThanAnEmptyRecord(t *testing.T) {
	_, err := summaryFromResponse("acme", refusal("UNAUTHENTICATED", "access context not available"))
	if err == nil {
		t.Fatal("a refusal decoded as a successful, empty access record")
	}
	if !strings.Contains(err.Error(), "access context not available") {
		t.Errorf("the cluster's own sentence was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "acme") {
		t.Errorf("the error does not name the cluster: %v", err)
	}
}

// The CODE is the difference between "something went wrong" and "your session
// was revoked" -- which is the single most likely reason to be running this
// command at all.
func TestSummaryFromResponseKeepsTheRefusalCode(t *testing.T) {
	_, err := summaryFromResponse("acme", refusal("UNAUTHENTICATED", "access context not available"))
	if err == nil {
		t.Fatal("a refusal decoded as success")
	}
	if !strings.Contains(err.Error(), "UNAUTHENTICATED") {
		t.Errorf("the refusal code was dropped: %v", err)
	}
}

// A refusal with no message must still be a refusal, not a blank.
func TestSummaryFromResponseHandlesARefusalWithNoMessage(t *testing.T) {
	_, err := summaryFromResponse("acme", refusal("PERMISSION_DENIED", ""))
	if err == nil {
		t.Fatal("an empty refusal decoded as success")
	}
	if !strings.Contains(err.Error(), "no reason given") {
		t.Errorf("an empty refusal message produced no sentence: %v", err)
	}
	if !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Errorf("the code was dropped: %v", err)
	}
}

// A response of the right family carrying nothing is not "you hold nothing".
func TestSummaryFromResponseRefusesAResponseWithNoAccessRecord(t *testing.T) {
	_, err := summaryFromResponse("acme", &memqlv1.MemqlServerMessage{})
	if err == nil {
		t.Fatal("a response with no access record decoded as an empty record")
	}
	if !strings.Contains(err.Error(), "without an access record") {
		t.Errorf("unexpected sentence: %v", err)
	}
}

func TestSummaryFromResponseRefusesNil(t *testing.T) {
	if _, err := summaryFromResponse("acme", nil); err == nil {
		t.Fatal("a nil response decoded as an empty record")
	}
}

// The happy path still decodes.
func TestSummaryFromResponseDecodesAnAccessRecord(t *testing.T) {
	got, err := summaryFromResponse("acme", &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_MyAccessResult{
			MyAccessResult: &memqlv1.MyAccessResult{
				UserId: "u_ada", PrimaryEmail: "ada@example.com",
			},
		},
	})
	if err != nil {
		t.Fatalf("summaryFromResponse: %v", err)
	}
	if got.UserID != "u_ada" {
		t.Errorf("UserID = %q, want u_ada", got.UserID)
	}
	// The pin carries `role` (memql#5181) since the 2026-09-08 bump, so the
	// field is ON the wire; a record that did not set it is one the cluster
	// sent EMPTY, which is the other fact and is reported as such.
	if !got.Role.OnTheWire {
		t.Error("Role.OnTheWire = false at the current pin, which carries the `role` field")
	}
	if got.Role.Reported {
		t.Error("Role.Reported = true for a record that set no role")
	}
}
