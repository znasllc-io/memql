package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

func continuityIntent(r preparationRecord, key string) (argocd.Intent, error) {
	for _, app := range r.Scope.applicationMembers() {
		if app.Key == key {
			return app.Intent, nil
		}
	}
	return argocd.Intent{}, errors.New("Application history is outside this preparation")
}

func decodeApplicationContinuity(body []byte, r *preparationRecord) error {
	if len(body) > 16<<10 || receiverJSON(body, &r.Continuity) != nil || r.Continuity == nil || len(r.Continuity) > len(r.Scope.applicationMembers()) {
		return errors.New("stored Application history is malformed")
	}
	encoded, _ := json.Marshal(r.Continuity)
	actual, actualErr := canonicalJSON(body)
	expected, expectedErr := canonicalJSON(encoded)
	if actualErr != nil || expectedErr != nil || !bytes.Equal(actual, expected) {
		return errors.New("stored Application history is noncanonical")
	}
	for key, state := range r.Continuity {
		intent, err := continuityIntent(*r, key)
		if err != nil || state.Validate(intent) != nil {
			return errors.New("stored Application history has changed scope")
		}
	}
	return nil
}

// advanceContinuity never holds a database transaction across a network read.
// The existing head/owner/workflow locks fence both sides. Competing valid
// readers retry from the winner's cursor; an observed real edit is sticky and
// cannot be overwritten by a slower successful reader on another replica.
func (j *preparationJournal) advanceContinuity(ctx context.Context, installation, preparation, key string, client *argocd.Client) (*argocd.ContinuityProof, error) {
	for attempt := 0; attempt < 3; attempt++ {
		r, err := j.get(ctx, installation, preparation)
		if err != nil {
			return nil, err
		}
		actor, err := preparationActor(ctx)
		if err != nil || actor != r.Scope.RequestedBy || (r.State != "preparing" && r.State != "promoted") {
			return nil, errors.New("Application history requires its active original preparation")
		}
		intent, err := continuityIntent(r, key)
		if err != nil {
			return nil, err
		}
		prior, exists := r.Continuity[key]
		if !exists {
			prior, err = argocd.NewContinuityState(intent)
			if err != nil {
				return nil, err
			}
		}
		next, proof, checkErr := client.CheckContinuity(ctx, intent, prior)
		if checkErr != nil && !next.Failed {
			return nil, checkErr
		}
		_, err = j.withRecordState(ctx, installation, preparation, r.Scope.WorkflowDigest, r.State, func(current *preparationRecord) error {
			stored, ok := current.Continuity[key]
			if !ok {
				stored = prior
			}
			if stored.Failed {
				return argocd.ErrChanged
			}
			if next.Failed {
				stored.Failed = true
				current.Continuity[key] = stored
				return nil
			}
			if !sameJSON(stored, prior) {
				return errChanged
			}
			current.Continuity[key] = next
			return nil
		})
		if errors.Is(err, errChanged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return proof, checkErr
	}
	return nil, errors.New("Application history advanced concurrently; resume the same request")
}

// A lifecycle guard, not a detached controller: its goroutine belongs to one
// admitted preparation/requalification call and is joined before that call
// returns or starts Argo effects. Cursor retention lets a later replica replay
// a gap; unavailable history refuses rather than establishing a new baseline.
type applicationContinuityRun struct {
	cancel                    context.CancelFunc
	done                      chan struct{}
	err                       error
	mu                        sync.Mutex
	journal                   *preparationJournal
	client                    *argocd.Client
	installation, preparation string
	keys                      []string
}

func startApplicationContinuity(ctx context.Context, cancelOperation context.CancelCauseFunc, journal *preparationJournal, client *argocd.Client, r preparationRecord, selected map[string]bool) (*applicationContinuityRun, error) {
	run := &applicationContinuityRun{journal: journal, client: client, installation: r.Scope.InstallationID, preparation: r.ID, done: make(chan struct{})}
	for _, app := range r.Scope.applicationMembers() {
		if app.Intent.FormatVersion == 2 && (selected == nil || selected[app.Key]) {
			run.keys = append(run.keys, app.Key)
		}
	}
	if len(run.keys) == 0 {
		run.cancel = func() {}
		close(run.done)
		return run, nil
	}
	if cancelOperation == nil {
		return nil, errors.New("Application history guard has no owning operation lifecycle")
	}
	if err := run.check(ctx); err != nil {
		return nil, err
	}
	watchContext, cancel := context.WithCancel(ctx)
	run.cancel = cancel
	go func() {
		defer close(run.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchContext.Done():
				return
			case <-ticker.C:
				if err := run.check(watchContext); err != nil {
					if watchContext.Err() != nil {
						return
					}
					run.err = err
					cancelOperation(err)
					return
				}
			}
		}
	}()
	return run, nil
}

func (r *applicationContinuityRun) check(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range r.keys {
		if _, err := r.journal.advanceContinuity(ctx, r.installation, r.preparation, key, r.client); err != nil {
			return err
		}
	}
	return nil
}

func (r *applicationContinuityRun) stop() error {
	if r == nil {
		return nil
	}
	r.cancel()
	<-r.done
	return r.err
}

func (h *preparationHost) applicationProof(ctx context.Context, plan preparedPlan, key string) (*argocd.ContinuityProof, error) {
	if plan.Preparation == nil {
		return nil, errors.New("Application history has no admitted preparation")
	}
	app, err := plan.application(key)
	if err != nil {
		return nil, err
	}
	r, err := h.journal.get(ctx, plan.InstallationID, plan.Preparation.ID)
	if err != nil {
		return nil, err
	}
	intent, err := continuityIntent(r, key)
	_, planID, planErr := plan.canonical()
	if err != nil || planErr != nil || r.PromotedPlanID != planID || !sameIntent(intent, app.Intent) || r.State != "promoted" {
		return nil, errors.New("Application history differs from its promoted intent")
	}
	client, err := argocd.New(h.api)
	if err != nil {
		return nil, err
	}
	return h.journal.advanceContinuity(ctx, plan.InstallationID, r.ID, key, client)
}
