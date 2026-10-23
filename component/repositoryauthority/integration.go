package repositoryauthority

import (
	"context"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"strings"
	"sync"
	"time"
)

type Integration struct {
	mu sync.RWMutex
	d  Deps
}

func New(d Deps) *Integration                  { return &Integration{d: d} }
func (i *Integration) Configure(f func(*Deps)) { i.mu.Lock(); defer i.mu.Unlock(); f(&i.d) }
func (i *Integration) deps() Deps              { i.mu.RLock(); defer i.mu.RUnlock(); return i.d }
func (i *Integration) now() time.Time {
	if f := i.deps().Now; f != nil {
		return f().UTC()
	}
	return time.Now().UTC()
}
func (i *Integration) IntegrationName() string { return "repositoryauthority" }

func (i *Integration) Access(ctx context.Context, sourceID string) (Access, error) {
	var out Access
	actor, err := person(ctx, "read", "app:pipelines")
	if err != nil {
		return out, err
	}
	d := i.deps()
	if d.Source == nil {
		return out, errors.New("source access is unavailable")
	}
	s, err := d.Source(ctx, sourceID, false)
	if err != nil {
		return out, err
	}
	a, err := i.access(ctx, s)
	if err != nil {
		return out, err
	}
	out = Access{SourceID: s.ID, Repository: s.Repository, RepositoryID: a.RepositoryID, CanWrite: same(actor, s.OwnerUserID) && a.CanWrite, Route: "unavailable"}
	r, err := i.resolveRoute(ctx, s, out.CanWrite)
	if err != nil {
		return out, nil
	}
	if _, err := person(ctx, "execute", RequestCapability); err != nil {
		return out, nil
	}
	out.CanRequest = true
	out.Operations = []string{OperationPropose}
	if r.ReviewTemplate != "" && r.DeliveryTemplate != "" {
		out.Operations = append(out.Operations, OperationDeliver)
	}
	out.Route = "maintainer"
	if r.Remote != "" {
		out.Route = "remote"
	} else if r.SourceID == s.ID && out.CanWrite {
		out.Route = "own"
	}
	return out, nil
}
func (i *Integration) Request(ctx context.Context, req Request) (Receipt, error) {
	actor, err := person(ctx, "execute", RequestCapability)
	if err != nil {
		return Receipt{}, err
	}
	if err = validateRequest(req); err != nil {
		return Receipt{}, err
	}
	d := i.deps()
	if d.Source == nil {
		return Receipt{}, errors.New("source access is unavailable")
	}
	s, err := d.Source(ctx, req.SourceID, false)
	if err != nil {
		return Receipt{}, err
	}
	s.Repository = strings.ToLower(s.Repository)
	a, err := i.access(ctx, s)
	if err != nil {
		return Receipt{}, err
	}
	r, err := i.resolveRoute(ctx, s, same(actor, s.OwnerUserID) && a.CanWrite)
	if err != nil {
		return Receipt{}, err
	}
	return i.open(ctx, actor, req, s, a.RepositoryID, r, false, same(actor, s.OwnerUserID) && a.CanWrite)
}

// Receive never imports authority from a sender. A configured upstream may
// accept this bounded proposal under the caller authenticated by its own stream.
func (i *Integration) Receive(ctx context.Context, t Transfer) (Receipt, error) {
	actor, err := person(ctx, "execute", RequestCapability)
	if err != nil {
		return Receipt{}, err
	}
	req := Request{RequestID: t.RequestID, Statement: t.Statement, Operation: t.Operation, Paths: t.Paths, ParentRequestID: t.ParentRequestID}
	if err = validateRequest(req); err != nil {
		return Receipt{}, err
	}
	if !repositoryName.MatchString(t.Repository) || t.RepositoryID <= 0 {
		return Receipt{}, errors.New("remote request requires a stable repository identity")
	}
	s := Source{Repository: strings.ToLower(t.Repository)}
	r, err := i.resolveRoute(ctx, s, false)
	if err != nil {
		return Receipt{}, err
	}
	if r.Remote != "" {
		return Receipt{}, errors.New("this peer is not the configured terminal maintainer for this repository")
	}
	req.SourceID = r.SourceID
	return i.open(ctx, actor, req, s, t.RepositoryID, r, true, false)
}
func (i *Integration) open(ctx context.Context, actor string, req Request, s Source, repoID int64, r route, remote bool, canWrite bool) (Receipt, error) {
	d := i.deps()
	if req.Operation == OperationDeliver && (r.ReviewTemplate == "" || r.DeliveryTemplate == "" || (r.Remote == "" && (len(r.ReviewPrompt) == 0 || len(r.DeliveryRecipe) == 0))) {
		return Receipt{}, errors.New("this route permits draft proposals only; delivery policy is not configured")
	}
	rec := record{ID: "change-" + digest(map[string]any{"actor": actor, "request": req.RequestID}), Request: req, Requester: actor, Source: s, RepositoryID: repoID, Route: r, State: "opening", RequesterCanWrite: canWrite, CreatedAt: i.now(), ExpiresAt: i.now().Add(time.Duration(r.TTLSeconds) * time.Second)}
	if r.Remote == "" {
		if d.Source == nil || d.OpenGoal == nil {
			return Receipt{}, errors.New("maintainer execution is unavailable")
		}
		maintainer, err := d.Source(ctx, r.SourceID, true)
		if err != nil {
			return Receipt{}, err
		}
		maintainer.OwnerUserID = memql.BareShortId(maintainer.OwnerUserID)
		a, err := i.access(ctx, maintainer)
		if err != nil {
			return Receipt{}, err
		}
		if !a.CanWrite || a.RepositoryID != repoID || !strings.EqualFold(maintainer.Repository, s.Repository) {
			return Receipt{}, errors.New("configured maintainer has no current write authority for this repository identity")
		}
		rec.Maintainer = maintainer
		rec.Immediate = !remote && same(actor, s.OwnerUserID) && same(actor, maintainer.OwnerUserID) && s.ID == maintainer.ID
	} else if d.RemoteRequest == nil {
		return Receipt{}, errors.New("configured request-only peer is unavailable")
	}
	rec.Fingerprint = digest(map[string]any{"request": req, "requester": actor, "source": s, "repositoryId": repoID, "route": r, "maintainer": rec.Maintainer, "immediate": rec.Immediate})
	saved, err := i.create(ctx, rec)
	if err != nil {
		return Receipt{}, err
	}
	rec = saved
	if terminal(rec.State) || rec.GoalPublished {
		return rec.receipt(actor), nil
	}
	if r.Remote != "" {
		result, err := d.RemoteRequest(ctx, rec.Route.Remote, rec.Route.RemoteBinding, Transfer{RequestID: rec.ID, Repository: s.Repository, RepositoryID: repoID, Statement: req.Statement, Operation: req.Operation, Paths: req.Paths, ParentRequestID: rec.ID})
		if err != nil {
			return rec.receipt(actor), err
		}
		if result.ID == "" || result.RepositoryID != repoID || !strings.EqualFold(result.Repository, s.Repository) {
			return Receipt{}, errors.New("remote receipt does not match the requested repository")
		}
		rec, err = i.update(ctx, rec.ID, func(current *record) error {
			if current.State != "opening" {
				return nil
			}
			current.RemoteRequestID = result.ID
			current.GoalPublished = true
			current.State = "pending"
			current.GoalID = result.GoalID
			current.RunID = result.RunID
			current.ApprovalID = result.ApprovalID
			return nil
		})
		return rec.receipt(actor), err
	}
	input := map[string]any{"requestId": rec.ID, "repository": s.Repository, "repositoryId": repoID, "statement": req.Statement, "operation": req.Operation, "paths": req.Paths, "parentRequestId": req.ParentRequestID}
	recipe, err := workflowhost.SnapshotFromMap(rec.Route.Recipe)
	if err != nil {
		return rec.receipt(actor), err
	}
	if err := recipe.CheckArgs(input); err != nil {
		return rec.receipt(actor), fmt.Errorf("repository recipe input is invalid: %w", err)
	}
	subject := map[string]any{"reviewType": ReviewType, "requestId": rec.ID, "fingerprint": rec.Fingerprint, "repository": s.Repository, "repositoryId": repoID, "operation": req.Operation, "paths": req.Paths, "statement": req.Statement, "expiresAt": rec.ExpiresAt.Format(time.RFC3339Nano)}
	receipt, err := d.OpenGoal(ctx, GoalRequest{Key: rec.ID, OwnerUserID: rec.Maintainer.OwnerUserID, Template: r.Template, Statement: req.Statement, Input: input, Subject: subject, Immediate: rec.Immediate, BeforeRun: func(c context.Context, goal, run string) error {
		_, e := i.update(c, rec.ID, func(current *record) error {
			if terminal(current.State) || !i.now().Before(current.ExpiresAt) {
				return errors.New("request was revoked or expired before goal publication")
			}
			if current.GoalID != "" && (current.GoalID != goal || current.RunID != run) {
				return errors.New("request already belongs to another execution")
			}
			current.GoalID, current.RunID = goal, run
			if current.Immediate {
				current.State = "approved"
			} else {
				current.State = "pending"
			}
			return nil
		})
		return e
	}})
	if err != nil {
		return rec.receipt(actor), err
	}
	rec, err = i.update(ctx, rec.ID, func(current *record) error {
		if terminal(current.State) {
			return nil
		}
		if current.GoalID != receipt.GoalID || current.RunID != receipt.RunID || current.GoalID == "" {
			return errors.New("goal was not durably bound before publication")
		}
		current.ApprovalID = receipt.ApprovalID
		current.GoalPublished = true
		if current.Immediate {
			current.State = "approved"
		} else {
			current.State = "pending"
		}
		return nil
	})
	return rec.receipt(actor), err
}
func (i *Integration) currentAuthority(ctx context.Context, r record) (Authorization, error) {
	var out Authorization
	if r.Route.Remote != "" || terminal(r.State) || !i.now().Before(r.ExpiresAt) {
		return out, errors.New("request is remote, expired or no longer authorized")
	}
	// A changed operator binding revokes this delegation; never substitute its new
	// template into the saved request. The old goal remains visible for explanation.
	route, err := i.resolveRoute(ctx, r.Source, r.RequesterCanWrite)
	if err != nil {
		return out, err
	}
	if digest(route) != digest(r.Route) {
		return out, errors.New("maintainer routing changed since this request was reviewed")
	}
	d := i.deps()
	if d.Source == nil {
		return out, errors.New("source access is unavailable")
	}
	s, err := d.Source(ctx, r.Maintainer.ID, true)
	if err != nil {
		return out, err
	}
	if !same(s.OwnerUserID, r.Maintainer.OwnerUserID) || s.CredentialID != r.Maintainer.CredentialID || !strings.EqualFold(s.Repository, r.Source.Repository) {
		return out, errors.New("maintainer source ownership or connection changed")
	}
	a, err := i.access(ctx, s)
	if err != nil {
		return out, err
	}
	if !a.CanWrite || a.RepositoryID != r.RepositoryID {
		return out, errors.New("maintainer no longer has current write authority")
	}
	return Authorization{Source: s, RepositoryID: a.RepositoryID, RequestID: r.ID}, nil
}
func (i *Integration) ValidateReview(ctx context.Context, subject map[string]any) error {
	actor, err := person(ctx, "", "")
	if err != nil {
		return err
	}
	r, err := i.get(ctx, str(subject["requestId"]), actor)
	if err != nil {
		return err
	}
	if !same(actor, r.Maintainer.OwnerUserID) || r.Fingerprint != str(subject["fingerprint"]) || r.State != "pending" {
		return errors.New("review does not match this maintainer's pending request")
	}
	_, err = i.currentAuthority(ctx, r)
	return err
}
func (i *Integration) Status(ctx context.Context, id string) (Receipt, error) {
	actor, err := person(ctx, "read", "app:pipelines")
	if err != nil {
		return Receipt{}, err
	}
	r, err := i.get(ctx, id, actor)
	if err != nil {
		return Receipt{}, err
	}
	r, err = i.refresh(ctx, r)
	return r.receipt(actor), err
}
func (i *Integration) observe(ctx context.Context, r record) (record, error) {
	if terminal(r.State) {
		return r, nil
	}
	if !i.now().Before(r.ExpiresAt) {
		r.State = "expired"
		r.Reason = "The approved request window expired."
		return r, nil
	}

	d := i.deps()
	state, reason := r.State, r.Reason
	if r.Route.Remote != "" && r.RemoteRequestID != "" {
		if d.RemoteStatus == nil || d.RemoteBinding == nil || !sha256hex.MatchString(r.Route.RemoteBinding) {
			return r, errors.New("request-only peer status is unavailable")
		}
		binding, err := d.RemoteBinding(ctx, r.Route.Remote)
		if err != nil {
			return r, err
		}
		if binding != r.Route.RemoteBinding {
			return r, errors.New("repository peer connection or requester changed since this request was opened")
		}
		remote, err := d.RemoteStatus(ctx, r.Route.Remote, r.Route.RemoteBinding, r.RemoteRequestID)
		if err != nil {
			return r, err
		}
		if remote.ID != r.RemoteRequestID || remote.RepositoryID != r.RepositoryID || !strings.EqualFold(remote.Repository, r.Source.Repository) {
			return r, errors.New("remote status names a different request or repository")
		}
		state, reason = remote.State, remote.Reason
		r.DeliveryReceipt = remote.Delivery
		r.ReviewReceipt = remote.Review
		r.ProposalReceipt = remote.Proposal
	} else if !r.Immediate && r.ApprovalID != "" {
		if d.Decision == nil {
			return r, errors.New("approval status is unavailable")
		}
		decision, err := d.Decision(ctx, r.Maintainer.OwnerUserID, r.ApprovalID)
		if err != nil {
			return r, err
		}
		if decision.GoalID != r.GoalID || decision.RunID != r.RunID || decision.RequestID != r.ID || decision.Fingerprint != r.Fingerprint {
			return r, errors.New("approval belongs to another execution")
		}
		state, reason = decision.State, decision.Reason
	}
	if state != "approved" && state != "declined" && state != "pending" && state != "revoked" && state != "expired" && state != "opening" {
		return r, errors.New("approval status is not an authorized decision")
	}
	r.State, r.Reason = state, reason
	return r, nil
}
func (i *Integration) refresh(ctx context.Context, r record) (record, error) {
	observed, err := i.observe(ctx, r)
	if err != nil {
		return r, err
	}
	if observed.State == r.State && observed.Reason == r.Reason && digest(observed.DeliveryReceipt) == digest(r.DeliveryReceipt) && digest(observed.ReviewReceipt) == digest(r.ReviewReceipt) && digest(observed.ProposalReceipt) == digest(r.ProposalReceipt) {
		return r, nil
	}
	return i.update(ctx, r.ID, func(current *record) error {
		if !terminal(current.State) {
			current.State, current.Reason = observed.State, observed.Reason
			if current.Route.Remote != "" {
				current.DeliveryReceipt = observed.DeliveryReceipt
				current.ReviewReceipt = observed.ReviewReceipt
				current.ProposalReceipt = observed.ProposalReceipt
			}
		}
		return nil
	})
}
func (i *Integration) Revoke(ctx context.Context, id, reason string) (Receipt, error) {
	actor, err := person(ctx, "execute", RequestCapability)
	if err != nil {
		return Receipt{}, err
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 2000 {
		return Receipt{}, errors.New("revocation needs a bounded explanation")
	}
	r, err := i.get(ctx, id, actor)
	if err != nil {
		return Receipt{}, err
	}
	if !same(actor, r.Maintainer.OwnerUserID) {
		return Receipt{}, errors.New("only the configured maintainer can revoke this delegation")
	}
	if _, err = i.currentAuthority(ctx, r); err != nil {
		return Receipt{}, err
	}
	r, err = i.update(ctx, id, func(c *record) error { c.State, c.Reason = "revoked", reason; return nil })
	return r.receipt(actor), err
}
func (i *Integration) AuthorizeEffect(ctx context.Context, id, operation string, paths []string) (Authorization, error) {
	actor, err := person(ctx, "", "")
	if err != nil {
		return Authorization{}, err
	}
	r, err := i.get(ctx, id, actor)
	if err != nil {
		return Authorization{}, err
	}
	run, ok := common.RunFromContext(ctx)
	if !ok || run.Mode != common.RunModeLive || !same(run.GoalId, r.GoalID) || !same(run.RunId, r.RunID) || !same(run.OwnerUserId, r.Maintainer.OwnerUserID) || !same(actor, r.Maintainer.OwnerUserID) || !operationIncludes(r.Request.Operation, operation) || !containsPaths(r.Request.Paths, paths) {
		return Authorization{}, errors.New("effect is outside the approved execution, operation or path scope")
	}
	active := i.deps().ExecutionActive
	if active == nil {
		return Authorization{}, errors.New("live execution authority is unavailable")
	}
	if err := active(ctx, r.Maintainer.OwnerUserID, r.GoalID, r.RunID); err != nil {
		return Authorization{}, err
	}
	r, err = i.observe(ctx, r)
	if err != nil {
		return Authorization{}, err
	}
	if r.State != "approved" {
		return Authorization{}, fmt.Errorf("request is %s, not approved", r.State)
	}
	return i.currentAuthority(ctx, r)
}

// WithAuthorizedEffect serializes a bounded writer with revocation. The effect
// owns its idempotency receipt; an uncertain provider result must not be retried.
func (i *Integration) WithAuthorizedEffect(ctx context.Context, id, operation string, paths []string, effect func(Authorization) error) error {
	grant, err := i.AuthorizeEffect(ctx, id, operation, paths)
	if err != nil {
		return err
	}
	_, err = i.update(ctx, id, func(r *record) error {
		observed, err := i.observe(ctx, *r)
		if err != nil {
			return err
		}
		r.State, r.Reason = observed.State, observed.Reason
		if r.State != "approved" || !i.now().Before(r.ExpiresAt) {
			return errors.New("delegation changed before the effect")
		}
		return effect(grant)
	})
	return err
}

func operationIncludes(granted, effect string) bool {
	return granted == effect || (granted == OperationDeliver && effect == OperationPropose)
}
