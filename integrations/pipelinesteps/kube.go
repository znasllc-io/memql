package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// Kube is the runner's whole conversation with the Kubernetes API: a step's
// Job, its Secret, its pod and the pod's logs, in the pipelines namespace,
// through component/deploycontrol's ClusterAPI (client-go is banned from the
// module graph). Each method is one or two API calls and decides nothing about
// a step; the runner composes them.
//
// Three rules hold across the methods:
//
//   - A delete never orphans. Every delete asks for background propagation:
//     the API's default for a batch/v1 Job is to ORPHAN its pods, which would
//     leave the step running with its Job gone.
//   - Gone is done. Deleting what is already absent succeeds: a retry, a
//     second replica and the Job's TTL all delete the same objects.
//   - A Secret's values never leave this file. The API answers a create, a
//     patch and a delete of Secrets with the Secrets themselves, values
//     included, and those answers are dropped unread.
type Kube struct {
	api *deploycontrol.ClusterAPI
	ns  string
}

// NewKube talks to the API server through api, in namespace
// (Config.Namespace): the namespace whose Role grants the engine identity Jobs.
func NewKube(api *deploycontrol.ClusterAPI, namespace string) *Kube {
	return &Kube{api: api, ns: namespace}
}

const (
	contentJSON       = "application/json"
	contentMergePatch = "application/merge-patch+json"
)

// ErrContainerNotStarted is FollowLog's and TailLog's answer for a container
// with no log to read: it has not started yet -- the pod is initializing, its
// image is being pulled or cannot be, its configuration is refused -- or it
// ended without ever having run. The runner meets it before every step
// container starts, and for any service the step never reached. The API
// server's own answer is wrapped with it, so errors.As still finds the
// *deploycontrol.StatusError.
var ErrContainerNotStarted = errors.New("pipelinesteps: the container has not started, so it has no log")

// containerNoLog matches the kubelet's sentence for such a container, which
// the API server relays as a 400 Status (kubelet validateContainerLogStatus,
// measured on k3s v1.32): `container "step" in pod "x" is waiting to start:
// <reason>`, `... is waiting to start - no logs yet`, `... is not available`
// (no status yet) and `... is terminated` (it ended with no container ever
// created). A 400 that says anything else -- a container the pod does not
// have, a malformed parameter -- is the caller's mistake and stays one.
var containerNoLog = regexp.MustCompile(`^container "[^"]*" in pod "[^"]*" is (waiting to start|not available|terminated)`)

// CreateSecret creates the step's Secret. One that already exists is this
// step's -- its name is derived from the Job's -- left by an earlier attempt of
// the same step, and is used as it is.
func (k *Kube) CreateSecret(ctx context.Context, s Secret) error {
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("pipelinesteps: encoding secret %s: %w", s.Metadata.Name, err)
	}
	if _, err := k.api.Do(ctx, http.MethodPost, k.corePath("secrets", ""), contentJSON, body); err != nil && !deploycontrol.IsConflict(err) {
		return err
	}
	return nil
}

// OwnSecret makes the Job its Secret's owner, so that collecting the Job --
// the ack, the TTL, a cancel -- collects the clone token with it. Not as the
// Secret's controller, and without holding the Job's deletion until the Secret
// is gone. The merge patch replaces the whole list: the Secret has no other
// owner.
func (k *Kube) OwnSecret(ctx context.Context, secretName string, job Job) error {
	if job.Metadata.Name == "" || job.Metadata.UID == "" {
		return fmt.Errorf("pipelinesteps: job %q cannot own secret %s: an owner is named by its uid, and the Job has none (use the Job the API server returned)", job.Metadata.Name, secretName)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []OwnerReference{{
		APIVersion:         "batch/v1",
		Kind:               "Job",
		Name:               job.Metadata.Name,
		UID:                job.Metadata.UID,
		Controller:         ptr(false),
		BlockOwnerDeletion: ptr(false),
	}}}})
	if err != nil {
		return fmt.Errorf("pipelinesteps: encoding the owner of secret %s: %w", secretName, err)
	}
	_, err = k.api.Do(ctx, http.MethodPatch, k.corePath("secrets", secretName), contentMergePatch, patch)
	return err
}

// CreateJob creates the step's Job and returns it as the API server stored
// it, uid and resourceVersion included. A Job of that name that already exists
// is this step's -- the name is derived from (run, step, attempt) -- created by
// an earlier attempt of this runner or by another replica: it is read back and
// returned with created false, for the runner to adopt. Any other refusal (the
// ceiling's exceeded quota first among them) is returned as the API server
// gave it.
func (k *Kube) CreateJob(ctx context.Context, j Job) (job Job, created bool, err error) {
	body, err := json.Marshal(j)
	if err != nil {
		return Job{}, false, fmt.Errorf("pipelinesteps: encoding job %s: %w", j.Metadata.Name, err)
	}
	out, err := k.api.Do(ctx, http.MethodPost, k.jobsPath(""), contentJSON, body)
	if deploycontrol.IsConflict(err) {
		existing, getErr := k.GetJob(ctx, j.Metadata.Name)
		if getErr != nil {
			return Job{}, false, getErr
		}
		return existing, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	// The Job exists from here on, even when its answer cannot be read.
	stored, err := decodeJob(out, j.Metadata.Name)
	return stored, true, err
}

// GetJob reads the step's Job. An absent one is the API server's 404
// (deploycontrol.IsNotFound).
func (k *Kube) GetJob(ctx context.Context, name string) (Job, error) {
	out, err := k.api.Do(ctx, http.MethodGet, k.jobsPath(name), "", nil)
	if err != nil {
		return Job{}, err
	}
	return decodeJob(out, name)
}

// AnnotateJob sets annotations on the Job with a merge patch and returns the
// Job as patched. A non-empty resourceVersion makes it a compare-and-swap: the
// API server refuses it 409 (deploycontrol.IsConflict) when the Job changed
// since the runner read that version, which is how two replicas racing to take
// a Job over learn which one won. An empty one patches unconditionally (the
// heartbeat, the outcome).
func (k *Kube) AnnotateJob(ctx context.Context, name string, annots map[string]string, resourceVersion string) (Job, error) {
	if annots == nil {
		// A null in a merge patch deletes: "annotations": null would wipe the
		// persisted outcome and the runner's claim along with everything else.
		annots = map[string]string{}
	}
	meta := map[string]any{"annotations": annots}
	if resourceVersion != "" {
		meta["resourceVersion"] = resourceVersion
	}
	patch, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return Job{}, fmt.Errorf("pipelinesteps: encoding annotations for job %s: %w", name, err)
	}
	out, err := k.api.Do(ctx, http.MethodPatch, k.jobsPath(name), contentMergePatch, patch)
	if err != nil {
		return Job{}, err
	}
	return decodeJob(out, name)
}

// DeleteJob deletes the step's Job, and with it (background propagation) its
// pod and, once OwnSecret has run, its Secret. An absent Job is deleted.
func (k *Kube) DeleteJob(ctx context.Context, name string) error {
	return k.delete(ctx, k.jobsPath(name))
}

// DeleteSecret deletes the step's Secret. An absent Secret is deleted.
func (k *Kube) DeleteSecret(ctx context.Context, name string) error {
	return k.delete(ctx, k.corePath("secrets", name))
}

func (k *Kube) delete(ctx context.Context, path string) error {
	q := url.Values{"propagationPolicy": {"Background"}}
	// The answer is the deleted object -- for a Secret, its values -- or a
	// Status: dropped unread either way.
	if _, err := k.api.Do(ctx, http.MethodDelete, path+"?"+q.Encode(), "", nil); err != nil && !deploycontrol.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteRun deletes every Job and every Secret of a run, wherever they were
// created, by its run label (RunLabelValue, the value BuildJob labelled them
// with), and answers how many Jobs it matched. The Secrets are deleted even
// when the Jobs could not be: they hold the clone token and the step's
// secrets. A blank run id names no run and is refused.
//
// The API server answers a deletecollection with the list of what it deleted
// (measured). An answer too large for ClusterAPI.Do (8 MiB) to read whole is
// an error although the deletion happened; deleting again is safe.
func (k *Kube) DeleteRun(ctx context.Context, runID string) (int, error) {
	if strings.TrimSpace(runID) == "" {
		return 0, errors.New("pipelinesteps: a run's Jobs are selected by its run id, and there is none")
	}
	q := url.Values{
		"labelSelector":     {LabelRun + "=" + RunLabelValue(runID)},
		"propagationPolicy": {"Background"},
	}.Encode()

	matched := 0
	out, jobsErr := k.api.Do(ctx, http.MethodDelete, k.jobsPath("")+"?"+q, "", nil)
	if jobsErr == nil {
		var deleted struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(out, &deleted); err != nil {
			jobsErr = fmt.Errorf("pipelinesteps: the Jobs of run %s were deleted, but the answer could not be read: %w", runID, err)
		}
		matched = len(deleted.Items)
	}
	_, secretsErr := k.api.Do(ctx, http.MethodDelete, k.corePath("secrets", "")+"?"+q, "", nil)
	return matched, errors.Join(jobsErr, secretsErr)
}

// JobPod is the Job's pod, found by the job-name label the Job controller
// puts on it: the newest when more than one answers (a leftover of a deleted
// Job of the same name), nil before the controller has created one.
func (k *Kube) JobPod(ctx context.Context, jobName string) (*Pod, error) {
	q := url.Values{"labelSelector": {"job-name=" + jobName}}
	out, err := k.api.Do(ctx, http.MethodGet, k.corePath("pods", "")+"?"+q.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	var list PodList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("pipelinesteps: reading the pods of job %s: %w", jobName, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	newest := list.Items[0]
	for _, p := range list.Items[1:] {
		at, best := p.Metadata.CreationTimestamp, newest.Metadata.CreationTimestamp
		if at.After(best) || (at.Equal(best) && p.Metadata.Name > newest.Metadata.Name) {
			newest = p
		}
	}
	return &newest, nil
}

// FollowLog streams a container's log as it is written, every line prefixed
// by its RFC3339Nano timestamp, from since when since is set. The caller owns
// the reader and must Close it, and the caller's context is the stream's only
// bound (deploycontrol.ClusterAPI.Stream): pass one that is cancelled when the
// step ends or is abandoned.
//
// sinceTime is whole seconds on the wire -- the API server forwards it to the
// kubelet as RFC3339, whatever precision it was sent with (measured on k3s
// v1.32) -- so it is sent truncated, never rounded up past a line. A stream
// resumed from a cursor therefore starts at the cursor's second: lines stamped
// at or before the cursor come again, and the caller drops them by their
// timestamps.
//
// A container that has not started has no log yet: ErrContainerNotStarted. A
// pod the scheduler has not placed answers with an empty stream (204).
func (k *Kube) FollowLog(ctx context.Context, pod, container string, since time.Time) (io.ReadCloser, error) {
	q := url.Values{"container": {container}, "follow": {"true"}, "timestamps": {"true"}}
	if !since.IsZero() {
		q.Set("sinceTime", since.UTC().Format(time.RFC3339))
	}
	rc, err := k.api.Stream(ctx, k.corePath("pods", pod)+"/log?"+q.Encode())
	if err != nil {
		return nil, noLogYet(err)
	}
	return rc, nil
}

// TailLog is the last lines of a container's log, as text, not followed and
// not stamped: what the runner keeps of the clone and the services after a
// step ends. A container that never started has no log:
// ErrContainerNotStarted. The answer is read through ClusterAPI.Do, which
// stops at 8 MiB, so one enormous line is cut rather than held whole.
func (k *Kube) TailLog(ctx context.Context, pod, container string, lines int) (string, error) {
	if lines < 1 {
		return "", fmt.Errorf("pipelinesteps: a tail of %d lines asks for nothing", lines)
	}
	q := url.Values{"container": {container}, "tailLines": {strconv.Itoa(lines)}}
	out, err := k.api.Do(ctx, http.MethodGet, k.corePath("pods", pod)+"/log?"+q.Encode(), "", nil)
	if err != nil {
		return "", noLogYet(err)
	}
	return string(out), nil
}

// noLogYet turns the API server's answer for a container with no log to read
// into ErrContainerNotStarted, wrapped around that answer, and returns every
// other error as it is.
func noLogYet(err error) error {
	var se *deploycontrol.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		return err
	}
	var status struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(se.Body), &status) != nil || !containerNoLog.MatchString(status.Message) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotStarted, err)
}

// jobsPath is the Jobs collection of the namespace, or one Job in it.
func (k *Kube) jobsPath(name string) string {
	return objectPath("apis/batch/v1/namespaces/"+url.PathEscape(k.ns)+"/jobs", name)
}

// corePath is a core/v1 collection of the namespace (secrets, pods), or one
// object in it.
func (k *Kube) corePath(plural, name string) string {
	return objectPath("api/v1/namespaces/"+url.PathEscape(k.ns)+"/"+plural, name)
}

func objectPath(collection, name string) string {
	if name == "" {
		return collection
	}
	return collection + "/" + url.PathEscape(name)
}

func decodeJob(body []byte, name string) (Job, error) {
	var j Job
	if err := json.Unmarshal(body, &j); err != nil {
		return Job{}, fmt.Errorf("pipelinesteps: reading job %s: %w", name, err)
	}
	return j, nil
}
