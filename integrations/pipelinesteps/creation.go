package pipelinesteps

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

const (
	annotCreation           = "memql.io/job-creation"
	annotCreationDefinition = "memql.io/job-creation-definition"
	creationQueued          = "queued"
)

var errCreationUncertain = errors.New("the attempt has no durable proof that Job creation has not started")

// A queued Secret is positive evidence, unlike an absent Job. Before EVERY
// Job POST the creator changes queued to its unique claim by resourceVersion.
// Only an explicit quota/throttling rejection permits returning it to queued. A lost
// response or lost creator leaves creating, which recovery never replays.
// Existing attempts without this protocol remain conservatively uncertain.
func creationDefinition(run StepRun) string {
	run.RecoverOnly = false
	// Remaining timeout is recomputed on a forward. RunDeadline and the
	// driver's pinned execution definition still bound the attempt.
	run.TimeoutSeconds, run.DeadlineCode = 0, ""
	keys := make(map[string]string, len(run.Secrets))
	for name := range run.Secrets {
		keys[name] = ""
	}
	run.Secrets = keys // bind secret names, never hash their values
	encoded, _ := json.Marshal(run)
	return string(namesEngine.MustFromMap(map[string]any{"queuedStep": string(encoded)}))
}

func (s *step) creationMetadata() (ObjectMeta, error) {
	meta, err := s.r.kube.SecretMetadata(s.ctx, SecretName(s.jobName))
	if deploycontrol.IsNotFound(err) {
		return ObjectMeta{}, errCreationUncertain
	}
	if err != nil {
		return ObjectMeta{}, err
	}
	if meta.UID == "" || meta.ResourceVersion == "" || !meta.DeletionTimestamp.IsZero() ||
		len(meta.OwnerReferences) != 0 || meta.Annotations[annotCreationDefinition] != creationDefinition(s.run) {
		return ObjectMeta{}, errCreationUncertain
	}
	return meta, nil
}

func (s *step) creationFresh(state string) bool {
	parts := strings.Fields(state)
	if len(parts) != 3 || parts[0] != "creating" {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, parts[2])
	return err == nil && s.r.fresh(at)
}

// claimCreation answers false when another creator or a metadata update won
// the race. The caller reads the Job again before trying any further effect.
func (s *step) claimCreation() (bool, error) {
	meta, err := s.creationMetadata()
	if err != nil {
		return false, err
	}
	state := meta.Annotations[annotCreation]
	if s.creationSent != "" && state == s.creationSent {
		return true, nil
	}
	if state != creationQueued {
		if s.creationFresh(state) {
			return false, nil
		}
		return false, errCreationUncertain
	}
	s.creationSent = "creating " + rand.Text() + " " + s.r.now().UTC().Format(time.RFC3339Nano)
	err = s.r.kube.SetCreationState(s.ctx, meta, s.creationSent)
	if deploycontrol.IsConflict(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *step) releaseRejectedCreation() error {
	var last error
	for range apiAttempts {
		meta, err := s.creationMetadata()
		if err != nil {
			if !transient(err) {
				return err
			}
			last = err
			if !s.sleep(s.r.cfg.PollInterval) {
				return s.ctx.Err()
			}
			continue
		}
		if meta.Annotations[annotCreation] == creationQueued {
			return nil
		}
		if meta.Annotations[annotCreation] != s.creationSent {
			return errCreationUncertain
		}
		err = s.r.kube.SetCreationState(s.ctx, meta, creationQueued)
		if err == nil || (!deploycontrol.IsConflict(err) && !transient(err)) {
			return err
		}
		last = err
		// A successful patch can lose its response. Read the state again;
		// queued is sufficient, but another creator's claim is never ours.
		if !s.sleep(s.r.cfg.PollInterval) {
			return s.ctx.Err()
		}
	}
	return fmt.Errorf("recording a refused create remained unconfirmed: %w", last)
}

func rejectedJobCreate(err error) bool {
	var status *deploycontrol.StatusError
	return errors.As(err, &status) && status.Method == http.MethodPost &&
		status.Code >= 400 && status.Code < 500 && status.Code != http.StatusRequestTimeout
}

func throttledJobCreate(err error) bool {
	var status *deploycontrol.StatusError
	return errors.As(err, &status) && status.Method == http.MethodPost && status.Code == http.StatusTooManyRequests
}

// SetCreationState mutates metadata only, fenced by both object identity and
// revision. A deleted/recreated Secret is never a successor this caller owns.
func (k *Kube) SetCreationState(ctx context.Context, meta ObjectMeta, state string) error {
	if err := named("secret", meta.Name); err != nil {
		return err
	}
	if meta.UID == "" || meta.ResourceVersion == "" {
		return fmt.Errorf("creation state requires observed Secret identity and revision")
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": meta.UID, "resourceVersion": meta.ResourceVersion,
		"annotations": map[string]string{annotCreation: state},
	}})
	if err != nil {
		return err
	}
	_, err = k.api.Do(ctx, http.MethodPatch, k.corePath("secrets", meta.Name), contentMergePatch, patch)
	return err
}
