//go:build agent

package app

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	work "github.com/znasllc-io/memql/integrations/work"
)

// PrepareDispatch places an owner's Apps work before spending its shared
// claim. A whole-step App session cannot move after admission: only the agent
// holding the actual computer stream can open it. Doors is a read-only probe;
// it neither selects an App (which advances the owner's cursor) nor grants
// computer tools. The normal execution and App gates still decide both.
func (d *workRunDispatcher) PrepareDispatch(ctx context.Context, req work.DispatchRequest) (work.DispatchRequest, bool) {
	inspectApps := req.AppRouteHint || req.Recovery || (req.OwnerUserId == "" && req.GoalId == "" && req.AutomationName == "")
	req, ready := d.prepareHumanDispatch(ctx, req)
	if !ready {
		return req, false
	}
	if !inspectApps {
		return req, true
	}
	read, cancel := context.WithTimeout(memql.ContextWithFreshRead(ctx), 2*time.Second)
	defer cancel()
	j, err := automations.LoadRunJournal(read, d.app.engine, req.RunId)
	if err != nil {
		return req, false
	}
	selector := work.AppDispatchSource(j.Routing, j.OwnerUserId)
	if selector == "" || j.GoalId == "" {
		return req, true
	}
	req.AppPlacement = true
	if req.OwnerUserId != "" && memql.BareShortId(req.OwnerUserId) != memql.BareShortId(j.OwnerUserId) {
		return req, false
	}
	if j.CancelRequested || work.IsDriverOwnedRun(j.TriggeredBy) {
		return req, false
	}
	// Preserve the existing id-only hint contract; fill identity only from the
	// owned journal, never from the recovery event's assertion.
	if req.OwnerUserId == "" && req.GoalId == "" && req.AutomationName == "" {
		req.OwnerUserId, req.GoalId, req.AutomationName = j.OwnerUserId, j.GoalId, j.AutomationName
		req.HumanResumeId = j.HumanResumeId
		if j.Rerun != nil {
			req.RerunRequestId = j.Rerun.RequestId
		}
	}
	if !workRerunServable(req, j) || !workRunCanStart(req, j, time.Now()) {
		return req, false
	}
	// Replays can carry several historical step overrides. Do not guess a
	// single placement for that mixed-source run. A targeted retry has one
	// explicit current override, which is honored without changing it.
	if j.ForkedFromRunId != "" || j.Mode == "replay" {
		return req, true
	}
	if j.Rerun != nil && j.Rerun.Override != nil && j.Rerun.Override.Model != "" {
		model := strings.TrimSpace(j.Rerun.Override.Model)
		if !strings.HasPrefix(model, "app:") {
			return req, true
		}
		selector = strings.Split(strings.TrimPrefix(model, "app:"), ":")[0]
	}
	doors, ok := d.app.appInference.(interface {
		Doors(context.Context, string) ([]memql.AppDoor, error)
	})
	if !ok {
		return req, true
	} // The existing execution path reports its missing provider.
	read, err = auth.ContextWithPersistedOwner(read, j.OwnerUserId, j.ExecutionAuthority,
		auth.NewIdentityResolver(auth.QueryRunnerFunc(func(ctx context.Context, query string) (any, error) {
			result, err := d.app.engine.Execute(ctx, query)
			if err != nil || result == nil {
				return nil, err
			}
			return result.OutputPayload(), nil
		}), d.app.Logger))
	if err != nil {
		return req, false
	}
	available, err := doors.Doors(read, j.OwnerUserId)
	if err != nil {
		return req, false
	} // Unknown is not an empty fleet or consent.
	return req, appDispatchHere(selector, available)
}

func appDispatchHere(selector string, doors []memql.AppDoor) bool {
	peer := false
	for _, door := range doors {
		if selector != "*" && selector != door.AppId {
			continue
		}
		if door.Runnable() {
			return true
		}
		peer = peer || door.Reachable()
	}
	// A known peer takes the existing claim instead. If no eligible App exists
	// anywhere, allow the ordinary NoModel result rather than strand the run or
	// silently choose paid inference. The probe is not a reservation; revocation
	// and stream loss are rechecked by the real execution gate.
	return !peer
}
