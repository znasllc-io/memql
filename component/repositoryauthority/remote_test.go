package repositoryauthority

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func configureRequestPeer(f *fixture, binding *string) {
	f.i.Configure(func(d *Deps) {
		d.WorkflowSource = func(kind, name string) (string, error) {
			if kind != "automation" || name != "authorityPolicy" {
				return "", errors.New("unexpected remote workflow")
			}
			return `@template
automation authorityPolicy {
 args {repository string! sourceId string! sourceOwnerId string! requesterCanWrite bool! bindings object!}
 return {remote: "maintainer", template: "changeRecipe", ttlSeconds: 3600}
}`, nil
		}
		d.RemoteBinding = func(context.Context, string) (string, error) { return *binding, nil }
	})
}

func TestRemoteLostResponseCannotRebindRequestOnAnotherReplica(t *testing.T) {
	sender, first, second := newFixture(t), newFixture(t), newFixture(t)
	binding := strings.Repeat("a", 64)
	configureRequestPeer(sender, &binding)
	peer, calls, loseReply := first, 0, true
	sender.i.Configure(func(d *Deps) {
		d.RemoteRequest = func(ctx context.Context, name, expected string, transfer Transfer) (Receipt, error) {
			if name != "maintainer" || expected != binding {
				return Receipt{}, errors.New("peer connection changed before dispatch")
			}
			calls++
			r, err := peer.i.Receive(actor(peer.requester), transfer)
			if err == nil && loseReply {
				return Receipt{}, errors.New("reply lost after remote goal creation")
			}
			return r, err
		}
	})
	req := sender.request("request-source")
	pending, err := sender.i.Request(actor(sender.requester), req)
	if err == nil || pending.ID == "" || first.opens != 1 || calls != 1 {
		t.Fatalf("uncertain initial request: %+v %v goals=%d calls=%d", pending, err, first.opens, calls)
	}
	stored, err := sender.i.get(context.Background(), pending.ID, sender.requester)
	if err != nil || stored.Route.RemoteBinding != binding || stored.GoalPublished {
		t.Fatalf("peer identity was not durably captured before send: %+v %v", stored, err)
	}

	// A new replica must not send the same local identity to another host after
	// its first host already created work but the caller never saw the receipt.
	binding, peer, loseReply = strings.Repeat("b", 64), second, false
	other := New(sender.i.deps())
	if _, err := other.Request(actor(sender.requester), req); err == nil {
		t.Fatal("an uncertain request was rebound to another peer")
	}
	if second.opens != 0 || calls != 1 {
		t.Fatalf("rebound peer received a duplicate request: goals=%d calls=%d", second.opens, calls)
	}

	// Restoring the original connection permits exact recovery. The real
	// receiving journal retains one goal across both calls and both replicas.
	binding, peer = strings.Repeat("a", 64), first
	recovered, err := other.Request(actor(sender.requester), req)
	if err != nil || recovered.ID != pending.ID || recovered.State != "pending" || first.opens != 1 || calls != 2 {
		t.Fatalf("same-peer recovery: %+v %v goals=%d calls=%d", recovered, err, first.opens, calls)
	}
	if _, err := other.Request(actor(sender.requester), req); err != nil || calls != 2 {
		t.Fatalf("confirmed request repeated the remote effect: %v calls=%d", err, calls)
	}
}

func TestRemoteStatusRequiresSavedPeerAndExactRequestIdentity(t *testing.T) {
	f := newFixture(t)
	binding := strings.Repeat("a", 64)
	configureRequestPeer(f, &binding)
	remote := Receipt{ID: "exact-remote-request", Repository: "core/engine", RepositoryID: 123, State: "pending"}
	statusCalls := 0
	f.i.Configure(func(d *Deps) {
		d.RemoteRequest = func(context.Context, string, string, Transfer) (Receipt, error) { return remote, nil }
		d.RemoteStatus = func(_ context.Context, peer, expected, id string) (Receipt, error) {
			if peer != "maintainer" || expected != binding || id != "exact-remote-request" {
				return Receipt{}, errors.New("status lost its saved connection or request")
			}
			statusCalls++
			return remote, nil
		}
	})
	r, err := f.i.Request(actor(f.requester), f.request("request-source"))
	if err != nil {
		t.Fatal(err)
	}
	other := New(f.i.deps())
	binding = strings.Repeat("b", 64)
	if _, err := other.Status(actor(f.requester), r.ID); err == nil || statusCalls != 0 {
		t.Fatalf("status reached a repointed peer: %v calls=%d", err, statusCalls)
	}
	binding = strings.Repeat("a", 64)
	remote.ID, remote.State = "different-request-same-repository", "approved"
	if _, err := other.Status(actor(f.requester), r.ID); err == nil {
		t.Fatal("another request's approval was accepted as this request's status")
	}
	stored, err := f.i.get(context.Background(), r.ID, f.requester)
	if err != nil || stored.State != "pending" {
		t.Fatalf("wrong receipt changed durable status: %s %v", stored.State, err)
	}
	remote.ID = "exact-remote-request"
	if got, err := other.Status(actor(f.requester), r.ID); err != nil || got.State != "approved" {
		t.Fatalf("exact status refused: %+v %v", got, err)
	}
	_, err = f.i.update(context.Background(), r.ID, func(r *record) error { r.Route.RemoteBinding = ""; return nil })
	if err != nil {
		t.Fatal(err)
	}
	before := statusCalls
	if _, err := other.Status(actor(f.requester), r.ID); err == nil || statusCalls != before {
		t.Fatal("a legacy unbound request silently acquired current peer authority")
	}
}

func TestRemoteReviewAndDeliveryReceiptsRefreshWithoutDecisionChange(t *testing.T) {
	f := newFixture(t)
	binding := strings.Repeat("a", 64)
	configureRequestPeer(f, &binding)
	remote := Receipt{ID: "upstream-request", Repository: "core/engine", RepositoryID: 123, State: "approved"}
	f.i.Configure(func(d *Deps) {
		d.RemoteRequest = func(context.Context, string, string, Transfer) (Receipt, error) { return remote, nil }
		d.RemoteStatus = func(context.Context, string, string, string) (Receipt, error) { return remote, nil }
	})
	r, err := f.i.Request(actor(f.requester), f.request("request-source"))
	if err != nil {
		t.Fatal(err)
	}
	remote.Proposal = &ProposalReceipt{State: "complete", Branch: "memql/change-exact", Commit: strings.Repeat("c", 40), PullRequestURL: "https://github.com/core/engine/pull/42"}
	remote.Review = &ReviewReceipt{ProposalHash: strings.Repeat("b", 64), Approved: true, Reason: "Exact proposed content reviewed."}
	remote.Delivery = &DeliveryReceipt{State: "queued", HeadSHA: strings.Repeat("c", 40), QueueEntryID: "entry-1"}
	remote.GoalID, remote.RunID, remote.ApprovalID = "private-goal", "private-run", "private-approval"
	other := New(f.i.deps())
	status, err := other.Status(actor(f.requester), r.ID)
	if err != nil || status.Proposal == nil || status.Proposal.PullRequestURL != remote.Proposal.PullRequestURL || status.Review == nil || !status.Review.Approved || status.Delivery == nil || status.Delivery.QueueEntryID != "entry-1" || status.GoalID != "" || status.RunID != "" || status.ApprovalID != "" {
		t.Fatalf("remote response lost delivery or leaked journal identity: %+v %v", status, err)
	}
	saved, err := f.i.get(context.Background(), r.ID, f.requester)
	if err != nil || saved.DeliveryReceipt == nil || saved.DeliveryReceipt.State != "queued" || saved.ReviewReceipt == nil || saved.ProposalReceipt == nil || saved.ProposalReceipt.PullRequestURL != remote.Proposal.PullRequestURL {
		t.Fatalf("same-decision update was not durable: %+v %v", saved, err)
	}
}
