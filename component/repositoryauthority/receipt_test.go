package repositoryauthority

import "testing"

func TestReceiptLinksOnlyToLocalOwnedJournal(t *testing.T) {
	r := record{ID: "request", Maintainer: Source{OwnerUserID: "maintainer"}, GoalID: "goal", RunID: "run", ApprovalID: "approval", State: "pending"}
	owned := r.receipt("v1:identity:user:maintainer")
	if owned.GoalID != "goal" || owned.RunID != "run" || owned.ApprovalID != "approval" {
		t.Fatal("local maintainer lost readable journal links")
	}
	for _, actor := range []string{"requester", ""} {
		got := r.receipt(actor)
		if got.GoalID != "" || got.RunID != "" || got.ApprovalID != "" || got.State != "pending" || got.ID != "request" {
			t.Fatalf("private journal linked from requester receipt: %#v", got)
		}
	}
	r.Route.Remote = "upstream"
	got := r.receipt("maintainer")
	if got.GoalID != "" || got.RunID != "" || got.ApprovalID != "" {
		t.Fatal("another installation's IDs became local Nexus links")
	}
}
