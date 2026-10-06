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

var errAttemptRetired = errors.New("the attempt already has a durable stop marker")

// The marker is separate from credentials and has no Job owner. Deleting a
// completed Job must not erase the fact that its original envelope is spent.
// No credentials are stored here. Maintenance removes it only after every
// supported run lifetime and after the execution resources have disappeared.
func (r *Runner) retireAttempt(ctx context.Context, jobName string) error {
	return r.createRetirement(ctx, jobName+retirementSuffix, "")
}

func runRetirementName(runID string) string {
	sum := namesEngine.MustFromMap(map[string]any{"retiredRun": runID})
	return "mp-" + string(sum)[:24] + runRetirementSuffix
}

func (r *Runner) createRetirement(ctx context.Context, name, runID string) error {
	marker := Secret{APIVersion: "v1", Kind: "Secret", Type: "Opaque", Metadata: ObjectMeta{
		Name: name, Namespace: r.cfg.Namespace,
		Labels:      map[string]string{LabelManagedBy: ManagedBy},
		Annotations: map[string]string{AnnotRunDeadline: r.now().Add(pl.ParseRunCeiling("1440")).UTC().Format(time.RFC3339Nano)},
	}}
	if runID != "" {
		marker.Metadata.Annotations[annotRetiredRun] = runID
	}
	if _, err := r.kube.CreateSecret(ctx, marker); err != nil {
		return err // a lost create response is reconciled on the next call
	}
	meta, err := r.kube.SecretMetadata(ctx, marker.Metadata.Name)
	if err != nil {
		return err
	}
	if meta.Labels[LabelManagedBy] != ManagedBy || meta.UID == "" || !meta.DeletionTimestamp.IsZero() || len(meta.OwnerReferences) != 0 {
		return fmt.Errorf("pipelinesteps: retirement marker %s has unexpected ownership", marker.Metadata.Name)
	}
	if meta.Annotations[annotRetiredRun] != runID {
		return errors.New("retirement marker does not bind the requested run")
	}
	return nil
}

func (s *step) checkRetirement() error {
	for _, name := range []string{s.jobName + retirementSuffix, runRetirementName(s.run.RunID)} {
		_, err := s.r.kube.SecretMetadata(s.ctx, name)
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
// while its create is unresolved. No collection DELETE can erase that evidence.
func (r *Runner) cleanupRun(ctx context.Context, runID string) (int, error) {
	if err := r.createRetirement(ctx, runRetirementName(runID), runID); err != nil {
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
