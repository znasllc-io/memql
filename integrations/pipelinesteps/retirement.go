package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

const retirementSuffix = "-retired"
const runRetirementSuffix = "-run-stop"
const annotRetiredRun = "memql.io/retired-run"

// latePostWindow is how long an attempt's stop marker must have stood before no
// Job POST can still land behind it. It is what lets a stopped attempt's cleanup
// finish over a creation claim that nothing else will ever resolve
// (cleanupReceipt, reapSecret).
//
// It runs from the marker because the marker is the fence: checkRetirement reads
// it last before every POST, so a POST leaves within one API call of a read that
// found none, whatever the age of its claim. A claim's stamp bounds nothing: a
// creator can spend over ten minutes after stamping it, retrying those reads
// against a hung API server, and still send.
//
// The slowest POST the fence lets through lands 180 s after the marker's stamp:
// 60 s for the marker's own write to commit (the API server's default
// --request-timeout), 30 s for the read's answer (deploycontrol's k8sRequestTimeout
// bounds a whole call), then 30 s on the client and 60 s on the API server for the
// POST. The other two minutes are margin for a proxy's buffer and for the
// difference between the API server's clock, which stamped the marker, and this
// replica's. Those 60 s are the documented default, measured on no cluster here.
//
// The window is what a person pays: a cancel that raced a POST reads as still
// stopping until this has passed and the next recovery pass has come. Two things
// can still beat it: a process frozen between that read and its POST, and a replica
// whose clock runs ahead of the API server's by more than the margin. The Job
// either one creates finds no Secret, so it has no clone token and none of the
// step's secrets. A step that declares a secret cannot start, and any other runs
// with no credentials. Its attempt's receipt is already written, so its result
// changes nothing, and the deadline and TTL it was created with collect it, a
// queued one once it is admitted.
const latePostWindow = 5 * time.Minute

// latePostPossible says a POST that passed the fence may still land behind this
// stop marker. A marker with no creation time cannot vouch for anything.
func (r *Runner) latePostPossible(marker ObjectMeta) bool {
	return marker.CreationTimestamp.IsZero() || r.now().Sub(marker.CreationTimestamp) < latePostWindow
}

var errAttemptRetired = errors.New("the attempt already has a durable stop marker")

// The marker is separate from credentials and has no Job owner. Deleting a
// completed Job must not erase the fact that its original envelope is spent.
// No credentials are stored here. Maintenance removes it only after every
// supported run lifetime and after the execution resources have disappeared.
//
// It answers the marker as it stands, the first stop's creation time included: a
// later cleanup's wait for a late POST runs from that moment, not from its own.
func (r *Runner) retireAttempt(ctx context.Context, jobName string) (ObjectMeta, error) {
	return r.createRetirement(ctx, jobName+retirementSuffix, "")
}

func runRetirementName(runID string) string {
	sum := namesEngine.MustFromMap(map[string]any{"retiredRun": runID})
	return "mp-" + string(sum)[:24] + runRetirementSuffix
}

func (r *Runner) createRetirement(ctx context.Context, name, runID string) (ObjectMeta, error) {
	marker := Secret{APIVersion: "v1", Kind: "Secret", Type: "Opaque", Metadata: ObjectMeta{
		Name: name, Namespace: r.cfg.Namespace,
		Labels:      map[string]string{LabelManagedBy: ManagedBy},
		Annotations: map[string]string{AnnotRunDeadline: r.now().Add(pl.ParseRunCeiling("1440")).UTC().Format(time.RFC3339Nano)},
	}}
	if runID != "" {
		marker.Metadata.Annotations[annotRetiredRun] = runID
	}
	if _, err := r.kube.CreateSecret(ctx, marker); err != nil {
		return ObjectMeta{}, err // a lost create response is reconciled on the next call
	}
	meta, err := r.kube.SecretMetadata(ctx, marker.Metadata.Name)
	if err != nil {
		return ObjectMeta{}, err
	}
	if meta.Labels[LabelManagedBy] != ManagedBy || meta.UID == "" || !meta.DeletionTimestamp.IsZero() || len(meta.OwnerReferences) != 0 {
		return ObjectMeta{}, fmt.Errorf("pipelinesteps: retirement marker %s has unexpected ownership", marker.Metadata.Name)
	}
	if meta.Annotations[annotRetiredRun] != runID {
		return ObjectMeta{}, errors.New("retirement marker does not bind the requested run")
	}
	return meta, nil
}

func (s *step) checkRetirement() error {
	// The attempt's marker is read last because cleanup times its wait from it
	// (latePostWindow), and each read below may retry for over five minutes. Read
	// first, it could pass, and the stop and the whole wait could then come and go
	// during the other read's retries, with the POST leaving after cleanup had
	// confirmed.
	for _, name := range []string{runRetirementName(s.run.RunID), s.jobName + retirementSuffix} {
		// A dropped read is not proof of either retirement or permission to
		// create. Use the same bounded, cancellable retry as the other API
		// gates; no creation effect occurs until both reads answer absence.
		err := s.retryAPI(func() error {
			_, err := s.r.kube.SecretMetadata(s.ctx, name)
			return err
		})
		if deploycontrol.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		return errAttemptRetired
	}
	return nil
}

// A run stop precedes the resource inventory. A request that has not made its
// credential Secret yet observes the stop before claiming creation; one that
// has claimed is visible in this inventory and cannot be declared cleaned up
// while its create may still land (latePostWindow). No collection DELETE can
// erase that evidence.
func (r *Runner) cleanupRun(ctx context.Context, runID string) (int, error) {
	if _, err := r.createRetirement(ctx, runRetirementName(runID), runID); err != nil {
		return 0, err
	}
	names, count, err := r.kube.runResourceNames(ctx, runID)
	if err != nil {
		return count, err
	}
	r.cancelLocalRun(runID)
	var errs []error
	for name := range names {
		var err error
		for {
			err = r.cleanupReceipt(ctx, name)
			if !deploycontrol.IsConflict(err) || !sleepCtx(ctx, 100*time.Millisecond) {
				break
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return count, errors.Join(errs...)
}

func (k *Kube) runResourceNames(ctx context.Context, runID string) (map[string]struct{}, int, error) {
	names := map[string]struct{}{}
	jobs := 0
	for _, kind := range []string{"jobs", "secrets"} {
		path := k.jobsPath("")
		if kind == "secrets" {
			path = k.corePath(kind, "")
		}
		q := url.Values{"labelSelector": {LabelRun + "=" + RunLabelValue(runID)}, "limit": {"100"}}
		for pages := 0; ; pages++ {
			if pages >= 100 {
				return names, jobs, errors.New("run cleanup inventory exceeds its bounded batch")
			}
			body, err := k.api.ListMetadata(ctx, path+"?"+q.Encode())
			if err != nil {
				return names, jobs, err
			}
			var list struct {
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
				Items []struct {
					Metadata ObjectMeta `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(body, &list); err != nil {
				return names, jobs, err
			}
			for _, item := range list.Items {
				meta := item.Metadata
				name := meta.Name
				if kind == "secrets" {
					name = strings.TrimSuffix(name, SecretName(""))
				} else {
					jobs++
				}
				if !isStepJobName(name) || meta.Labels[LabelManagedBy] != ManagedBy || meta.Labels[LabelRun] != RunLabelValue(runID) {
					return names, jobs, fmt.Errorf("run cleanup refuses unexpected %s ownership", kind)
				}
				names[name] = struct{}{}
			}
			if list.Metadata.Continue == "" {
				break
			}
			q.Set("continue", list.Metadata.Continue)
		}
	}
	return names, jobs, nil
}

// Called only when this creator knows it has not sent a POST. The separate
// retirement marker prevents any new claim after this unused claim is released.
func (s *step) releaseUnusedCreation() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), quickCallTimeout)
	defer cancel()
	meta, err := s.r.kube.SecretMetadata(ctx, SecretName(s.jobName))
	if err != nil {
		return err
	}
	if meta.Annotations[annotCreation] == creationQueued {
		return nil
	}
	if s.creationSent == "" || meta.Annotations[annotCreation] != s.creationSent {
		return errCreationUncertain
	}
	return s.r.kube.SetCreationState(ctx, meta, creationQueued)
}
