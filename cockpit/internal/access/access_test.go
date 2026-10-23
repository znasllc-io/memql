package access

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// A record that SETS no role must come back UNREPORTED -- not as an empty
// slug, and never as a legacy enum read through cluster_role. The two are
// different facts and the renderer says different things about them. (Until
// the 2026-09-08 pin bump the descriptor carried no `role` field at all; now
// it does, and an unset value is the cluster reporting nothing.)
func TestDecodeReportsNoRoleWhenTheClusterSetsNone(t *testing.T) {
	got := Decode(&memqlv1.MyAccessResult{
		UserId:       "u_ada",
		PrimaryEmail: "ada@example.com",
		DisplayName:  "Ada Lovelace",
		SessionId:    "sess_7",
	})

	if got.UserID != "u_ada" {
		t.Errorf("UserID = %q, want %q", got.UserID, "u_ada")
	}
	if got.PrimaryEmail != "ada@example.com" {
		t.Errorf("PrimaryEmail = %q, want %q", got.PrimaryEmail, "ada@example.com")
	}
	if got.DisplayName != "Ada Lovelace" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Ada Lovelace")
	}
	if got.SessionID != "sess_7" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess_7")
	}
	if got.Role.Reported {
		t.Errorf("Role.Reported = true, want false: the record set no role")
	}
	if got.Role.Slug != "" {
		t.Errorf("Role.Slug = %q, want empty", got.Role.Slug)
	}
}
