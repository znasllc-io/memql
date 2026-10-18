package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type toolSharingKey struct{}

// ContextWithToolSharing carries a receiving replica's checked workbench
// audience to the stream that counts the call. It grants no execution access.
func ContextWithToolSharing(ctx context.Context, subjects SharingSubjects) context.Context {
	return context.WithValue(ctx, toolSharingKey{}, subjects)
}

// SharingActivityStore publishes only counts from the replica holding the stream.
// The existing registration broadcast carries them to every BFF and open Fleet
// view; no additional browser subscription or per-recipient query is needed.
type SharingActivityStore interface {
	UpdateSharingActivity(context.Context, string, string, string, map[string]any) error
}

func (s *streamSession) markSharingChanged() {
	select {
	case s.sharingChanged <- struct{}{}:
	default:
	}
}

func (s *streamSession) sharingActivity() map[string]any {
	users, groups := map[string]int{}, map[string]int{}
	s.mu.Lock()
	// Host tools and signed-in app sessions are owner-only. Model calls
	// carry the admission decision made on this stream's replica, including
	// forwarded calls and legacy shares with no individual grant rows.
	ownerCalls, sharedCalls := len(s.sessions), 0
	workbenchUsers, workbenchGroups := map[string]int{}, map[string]int{}
	for callID := range s.pending {
		subjects := s.toolSharing[callID]
		if subjects.Borrowed {
			sharedCalls++
		} else {
			ownerCalls++
		}
		for _, subject := range subjects.UserIds {
			workbenchUsers[bareSubjectId(subject)]++
		}
		for _, subject := range subjects.GroupIds {
			workbenchGroups[bareSubjectId(subject)]++
		}
	}
	for _, call := range s.modelCalls {
		if call.sharingSubjects.Borrowed {
			sharedCalls++
		} else {
			ownerCalls++
		}
		for _, subject := range call.sharingSubjects.UserIds {
			users[bareSubjectId(subject)]++
		}
		for _, subject := range call.sharingSubjects.GroupIds {
			groups[bareSubjectId(subject)]++
		}
	}
	s.mu.Unlock()
	rows := func(counts map[string]int) []map[string]any {
		keys := make([]string, 0, len(counts))
		for key := range counts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]map[string]any, 0, len(keys))
		for _, key := range keys {
			out = append(out, map[string]any{"id": key, "activeCalls": counts[key]})
		}
		return out
	}
	return map[string]any{
		"nodeId": s.server.nodeId, "startedAt": s.sharingStartedAt.Format(time.RFC3339Nano),
		"observedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"ownerCalls": ownerCalls, "sharedCalls": sharedCalls,
		"users": rows(users), "groups": rows(groups),
		"workbench": map[string]any{"nodeId": s.server.nodeId, "startedAt": s.sharingStartedAt.Format(time.RFC3339Nano), "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "users": rows(workbenchUsers), "groups": rows(workbenchGroups)},
	}
}

// Coalesce bursts of starts/ends and periodically repair a missed write. The
// bounded channel never blocks inference; a failed publish is retried without
// claiming that a missing measurement means zero calls.
func (s *streamSession) runSharingActivity() {
	store, ok := s.server.store.(SharingActivityStore)
	if !ok {
		return
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	s.markSharingChanged()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		case <-s.sharingChanged:
		}
		timer := time.NewTimer(150 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		for {
			select {
			case <-s.sharingChanged:
				continue
			default:
			}
			break
		}
		if s.server.registry != nil && s.server.registry.WorkerById(s.worker.RegistrationId) != s.worker {
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		err := store.UpdateSharingActivity(ctx, s.worker.RegistrationId, s.worker.OwnerUserId, s.server.nodeId, s.sharingActivity())
		cancel()
		if err != nil && s.ctx.Err() == nil {
			s.server.logger.Warn("could not publish model sharing activity", "error", err)
		}
	}
}

// Fresh read + version fence keep a late former stream from replacing a newer
// stream's counts, including reconnects to the same replica. Every mutation's
// read-merge participates in the engine's row-version transaction lock.
func (s *EngineStore) UpdateSharingActivity(ctx context.Context, registrationID, ownerID, nodeID string, activity map[string]any) error {
	if s == nil || s.Engine == nil {
		return fmt.Errorf("worker.store: engine not configured")
	}
	ctx, err := ownerActor(ctx, ownerID)
	if err != nil {
		return err
	}
	ctx = auth.ContextWithInternalOrigin(memqlengine.ContextWithFreshRead(ctx))
	query, err := langparser.RenderCall("workersForUser", map[string]any{"ownerUserId": ownerID})
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		nodes, err := s.executeAndExtract(ctx, "query "+query)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			g := newWorkerFieldGetter(node)
			if !SameSubjectId(firstNonEmpty(g.str("id"), node.GetId()), registrationID) {
				continue
			}
			if g.str("connectedNodeId") != nodeID || g.str("revokedAt") != "" {
				return nil
			}
			prior := g.anyMap("sharingActivity")
			oldStart, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(prior["startedAt"]))
			newStart, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(activity["startedAt"]))
			if prior["nodeId"] == nodeID && oldStart.After(newStart) {
				return nil
			}
			if node.GetCreatedAt() == nil {
				return fmt.Errorf("worker.store: sharing activity requires a registration version")
			}
			fenced := memqlengine.ContextWithRowVersionFence(ctx, "v1:worker:registration", registrationID, node.GetCreatedAt().AsTime())
			call, err := langparser.RenderCall("updateWorkerSharingActivity", map[string]any{"registrationId": registrationID, "sharingActivity": activity})
			if err != nil {
				return err
			}
			_, err = s.Engine.Execute(fenced, call)
			if errors.Is(err, memqlengine.ErrRowVersionChanged) {
				break
			}
			return err
		}
	}
	return memqlengine.ErrRowVersionChanged
}
