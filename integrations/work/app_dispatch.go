package work

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// AppDispatchSource recognizes only a persisted, owner-selected Apps route.
// Auto and policy chains may fall back elsewhere and keep normal arbitration.
func AppDispatchSource(choice common.RouteChoice, owner string) string {
	if owner == "" || memql.BareShortId(choice.By) != memql.BareShortId(owner) {
		return ""
	}
	source := strings.TrimSpace(choice.Source)
	if source == "route:apps" || source == "app:*" {
		return "*"
	}
	if strings.HasPrefix(source, "app:") {
		return strings.Split(strings.TrimPrefix(source, "app:"), ":")[0]
	}
	return ""
}

func appOwnedRun(row map[string]any) bool {
	return rowString(row, "goalId") != "" && AppDispatchSource(common.RouteChoiceFrom(row["routing"]), rowString(row, "ownerUserId")) != ""
}

func appDispatchGate(runID string) string { return "app-dispatch:" + memql.BareShortId(runID) }

// appRecoverySnapshot binds a hint to the stored generation, consent and last
// real progress. No graph payload, authority, input or model content travels.
func appRecoverySnapshot(row map[string]any) string {
	values := map[string]any{}
	for _, key := range []string{"id", "ownerUserId", "goalId", "status", "automationName", "triggeredBy", "heartbeatAt", "startedAt", "createdAt", "routing", "rerun", "humanResumeId", "cancelRequested"} {
		values[key] = row[key]
	}
	data, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (i *Integration) claimAppRun(ctx context.Context, req DispatchRequest, claimer RunClaimer, key string, ttl time.Duration) bool {
	gateCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	release, err := i.decisionGate(gateCtx, appDispatchGate(req.RunId))
	if err != nil {
		return false
	}
	defer release()
	row, err := i.store().runForOwner(memql.ContextWithFreshRead(gateCtx), req.RunId)
	if err != nil || !appOwnedRun(row) || memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(req.OwnerUserId) ||
		!req.CanDispatchStoredRun(rowString(row, "goalId"), rowString(row, "status"), rowMap(row, "waitingOn"), i.clock()) || argBool(row, "cancelRequested") || IsDriverOwnedRun(rowString(row, "triggeredBy")) {
		return false
	}
	if req.RecoverySnapshot != "" {
		last, ok := lastHeartbeat(row)
		if !ok || last.After(i.clock().Add(-DefaultAbandonedAfterSeconds*time.Second)) || appRecoverySnapshot(row) != req.RecoverySnapshot {
			return false
		}
	}
	// A delayed graph event must not spend one generation's claim on another.
	if req.HumanResumeId != rowString(row, "humanResumeId") && (!req.Recovery || req.HumanResumeRecovery) {
		return false
	}
	if (!req.Recovery || req.HumanResumeRecovery) && req.RerunRequestId != rowString(rowMap(row, "rerun"), "requestId") {
		return false
	}
	// Graph and settled-answer callers historically supplied canonical and
	// bare run IDs. Preserve both claim spellings and their exact lease rules;
	// the shared bare-id decision gate prevents two aliases winning together.
	alias := req
	alias.RunId = appRunAlias(req.RunId)
	aliasKey, _ := runClaimKey(alias)
	observer, ok := claimer.(interface {
		HasLiveClaim(context.Context, string, []string, time.Duration) (bool, error)
	})
	if !ok {
		return false
	}
	live, err := observer.HasLiveClaim(gateCtx, runClaimName, []string{aliasKey}, ttl)
	if err != nil || live {
		return false
	}
	return claimer.ClaimWithTTL(gateCtx, runClaimName, key, ttl)
}

func (i *Integration) SetAppRecoveryPublisher(publish func(events.Event)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.appRecoveryPublisher == nil {
		i.appRecoveryPublisher = publish
	}
}

func (i *Integration) handleAppRecovery(ev events.Event) {
	id, owner, snapshot := rowString(ev.Payload, "id"), rowString(ev.Payload, "ownerUserId"), rowString(ev.Payload, "snapshot")
	if id == "" || owner == "" || len(snapshot) != 64 || i.dispatcherRef() == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		ctx, err := i.runOwnerActor(ctx, id, owner)
		if err != nil {
			return
		}
		row, err := i.store().runForOwner(memql.ContextWithFreshRead(ctx), id)
		if err != nil || !appOwnedRun(row) || memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(owner) || rowString(row, "status") != runStatusRunning || argBool(row, "cancelRequested") || appRecoverySnapshot(row) != snapshot {
			return
		}
		last, ok := lastHeartbeat(row)
		if !ok || last.After(i.clock().Add(-DefaultAbandonedAfterSeconds*time.Second)) {
			return
		}
		// The read timeout is not the execution lifetime. The ordinary dispatcher
		// restores authority again, claims under the gate and owns cancellation.
		i.dispatchRun(context.Background(), DispatchRequest{RunId: id, OwnerUserId: owner, GoalId: rowString(row, "goalId"), Status: runStatusRunning, TriggeredBy: rowString(row, "triggeredBy"), Recovery: true, RecoverySnapshot: snapshot})
	}()
}

type appClaimObserver interface {
	HasRecentClaim(context.Context, string, []string, time.Duration) (bool, error)
}

func appRunAlias(id string) string {
	bare := memql.BareShortId(id)
	if id == bare {
		return runConcept + ":" + bare
	}
	return bare
}

func appClaimKeys(row map[string]any) []string {
	id := rowString(row, "id")
	keys := []string{}
	rerun, answer := rowString(rowMap(row, "rerun"), "requestId"), rowString(row, "humanResumeId")
	for _, runID := range []string{id, appRunAlias(id)} {
		keys = append(keys, runID)
		if rerun != "" {
			keys = append(keys, runID+"#rerun:"+rerun)
		}
		if answer != "" {
			keys = append(keys, runID+"#answer:"+answer+"#rerun:"+rerun)
		}
	}
	return keys
}

// recoverAppRun offers recovery to existing run-delivery consumers and counts
// only an observed real claim as a redispatch. The final fresh check and write
// share the claim gate, closing the peer-claim/abandon race. An unresponsive
// claimant buys at most the normal heartbeat window, not a renewed lease.
func (i *Integration) recoverAppRun(ctx context.Context, stale map[string]any, now, cutoff time.Time) (claimed, abandoned bool, err error) {
	observer, ok := i.runClaimerRef().(appClaimObserver)
	if !ok {
		return false, false, fmt.Errorf("Apps recovery requires shared claim observation")
	}
	i.mu.RLock()
	publish := i.appRecoveryPublisher
	i.mu.RUnlock()
	if publish == nil {
		return false, false, fmt.Errorf("Apps recovery delivery unavailable")
	}
	id := rowString(stale, "id")
	snapshot := appRecoverySnapshot(stale)
	if snapshot == "" {
		return false, false, fmt.Errorf("Apps recovery snapshot unavailable")
	}
	publish(events.NewEvent(events.TopicWorkRunRecovery, events.KindMessage, map[string]any{"id": id, "ownerUserId": rowString(stale, "ownerUserId"), "snapshot": snapshot}))
	// Give the mesh fast path a bounded chance to claim. Durable delivery remains
	// the backstop; delivery alone is never reported as progress or execution.
	wait, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		fresh, readErr := observer.HasRecentClaim(wait, runClaimName, appClaimKeys(stale), DefaultAbandonedAfterSeconds*time.Second)
		if readErr != nil {
			return false, false, readErr
		}
		if fresh {
			return true, false, nil
		}
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			return i.finishAppRecovery(ctx, stale, cutoff, now, observer)
		case <-ticker.C:
		}
	}
}

func (i *Integration) finishAppRecovery(ctx context.Context, stale map[string]any, cutoff, now time.Time, observer appClaimObserver) (bool, bool, error) {
	gated, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	id := rowString(stale, "id")
	release, err := i.decisionGate(gated, appDispatchGate(id))
	if err != nil {
		return false, false, err
	}
	defer release()
	row, err := i.store().runForOwner(memql.ContextWithFreshRead(gated), id)
	if err != nil {
		return false, false, err
	}
	if !appOwnedRun(row) || rowString(row, "status") != runStatusRunning || argBool(row, "cancelRequested") || appRecoverySnapshot(row) != appRecoverySnapshot(stale) {
		return false, false, nil
	}
	last, ok := lastHeartbeat(row)
	if !ok || last.After(cutoff) {
		return false, false, nil
	}
	fresh, err := observer.HasRecentClaim(gated, runClaimName, appClaimKeys(row), DefaultAbandonedAfterSeconds*time.Second)
	if err != nil || fresh {
		return fresh, false, err
	}
	err = i.store().updateRun(gated, id, map[string]any{"status": runStatusAbandoned, "errorCode": "run_abandoned", "errorMessage": abandonedMessage(row, last), "finishedAt": rfc(now)})
	return false, err == nil, err
}
