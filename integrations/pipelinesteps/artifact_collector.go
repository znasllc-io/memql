package pipelinesteps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

const (
	artifactVolume     = "artifacts"
	artifactPath       = "/memql-artifacts"
	artifactFile       = artifactPath + "/completed.tar"
	ContainerArtifacts = "artifacts"
)

// The collector has no credentials, command environment, cache or checkout.
// It remains alive after the command ends so a replacement workbench can read
// the same export. The Job deadline and guarded receipt cleanup bound its life.
func artifactCollector(cfg Config, seconds int) Container {
	security := rootContext()
	security.ReadOnlyRootFilesystem = ptr(true)
	return Container{
		Name: ContainerArtifacts, Image: cfg.CloneImage, ImagePullPolicy: "IfNotPresent",
		Command:         []string{"sleep", strconv.Itoa(seconds)},
		VolumeMounts:    []VolumeMount{{Name: artifactVolume, MountPath: artifactPath, ReadOnly: true}},
		SecurityContext: security,
		Resources:       &Resources{Requests: map[string]string{"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "1Mi"}, Limits: map[string]string{"cpu": "250m", "memory": "64Mi", "ephemeral-storage": "16Mi"}},
	}
}

func artifactPrepContainer(cfg Config) Container {
	security := rootContext()
	security.ReadOnlyRootFilesystem = ptr(true)
	return Container{Name: "artifact-prep", Image: cfg.CloneImage,
		Command:         []string{"chmod", "1777", artifactPath},
		VolumeMounts:    []VolumeMount{{Name: artifactVolume, MountPath: artifactPath}},
		SecurityContext: security,
	}
}

// CollectArtifacts admits only the exact terminated producer and running
// collector observed by this runner. A label alone is never ownership. Exec
// has no UID precondition, so identity is re-read after the full transfer and
// uncertain bytes are discarded before any storage/publication side effect.
func (k *Kube) CollectArtifacts(ctx context.Context, run StepRun, observed *Pod, maxBytes int64) (*ArtifactSnapshot, error) {
	if observed == nil || observed.Metadata.UID == "" {
		return nil, errors.New("artifact collection has no observed pod identity")
	}
	jobName := JobName(run.RunID, run.StepKey, run.Attempt)
	job, err := k.GetJob(ctx, jobName)
	if err != nil {
		return nil, err
	}
	if !artifactJobOwned(job, run) {
		return nil, errors.New("artifact Job does not belong to the requested attempt")
	}
	before, err := k.artifactPod(ctx, job, observed)
	if err != nil {
		return nil, err
	}
	read, write := io.Pipe()
	transferCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := k.api.ReadPodFile(transferCtx, k.ns, before.Metadata.Name, ContainerArtifacts, artifactFile, write, maxBytes)
		_ = write.CloseWithError(err)
		done <- err
	}()
	snapshot, err := SnapshotArtifacts(ctx, read, run.Artifacts, maxBytes)
	_ = read.CloseWithError(err)
	if err != nil {
		cancel()
	}
	transferErr := <-done
	if err != nil || transferErr != nil {
		if snapshot != nil {
			_ = snapshot.Close()
		}
		return nil, errors.Join(err, transferErr)
	}
	afterJob, err := k.GetJob(ctx, jobName)
	if err == nil && (afterJob.Metadata.UID != job.Metadata.UID || !artifactJobOwned(afterJob, run)) {
		err = errors.New("artifact Job changed during transfer")
	}
	if err == nil {
		var after *Pod
		after, err = k.artifactPod(ctx, afterJob, before)
		if err == nil && !sameArtifactContainers(before, after) {
			err = errors.New("artifact producer or collector changed during transfer")
		}
	}
	if err != nil {
		_ = snapshot.Close()
		return nil, err
	}
	return snapshot, nil
}

func artifactJobOwned(job Job, run StepRun) bool {
	if job.Metadata.UID == "" || job.Metadata.Name != JobName(run.RunID, run.StepKey, run.Attempt) || !job.Metadata.DeletionTimestamp.IsZero() {
		return false
	}
	for key, value := range objectLabels(run) {
		if job.Metadata.Labels[key] != value {
			return false
		}
	}
	for _, key := range []string{AnnotOwner, AnnotWorkRun, AnnotStepKey} {
		if job.Metadata.Annotations[key] != objectAnnotations(run)[key] {
			return false
		}
	}
	return true
}

func (k *Kube) artifactPod(ctx context.Context, job Job, observed *Pod) (*Pod, error) {
	pods, err := k.JobPods(ctx, job.Metadata.Name)
	if err != nil {
		return nil, err
	}
	for n := range pods {
		pod := &pods[n]
		if pod.Metadata.Name != observed.Metadata.Name || pod.Metadata.UID != observed.Metadata.UID {
			continue
		}
		owned := false
		for _, ref := range pod.Metadata.OwnerReferences {
			if ref.APIVersion == "batch/v1" && ref.Kind == "Job" && ref.Name == job.Metadata.Name && ref.UID == job.Metadata.UID && ref.Controller != nil && *ref.Controller {
				owned = true
			}
		}
		if !owned || !pod.Metadata.DeletionTimestamp.IsZero() {
			return nil, errors.New("artifact pod has no live controller ownership")
		}
		producer, collector := podContainer(pod, false, ContainerStep), podContainer(pod, false, ContainerArtifacts)
		if producer == nil || producer.State.Terminated == nil || producer.ContainerID == "" || collector == nil || collector.State.Running == nil || collector.ContainerID == "" || collector.RestartCount != 0 {
			return nil, errors.New("artifact producer must have ended and its collector must still run")
		}
		var expected, actual *Container
		for n := range job.Spec.Template.Spec.Containers {
			if job.Spec.Template.Spec.Containers[n].Name == ContainerArtifacts {
				expected = &job.Spec.Template.Spec.Containers[n]
			}
		}
		for n := range pod.Spec.Containers {
			if pod.Spec.Containers[n].Name == ContainerArtifacts {
				actual = &pod.Spec.Containers[n]
			}
		}
		if expected == nil || actual == nil || !sameCollectorSpec(expected, actual) {
			return nil, errors.New("artifact collector differs from its owned Job")
		}
		if len(actual.Env) != 0 || !reflect.DeepEqual(actual.VolumeMounts, []VolumeMount{{Name: artifactVolume, MountPath: artifactPath, ReadOnly: true}}) || actual.SecurityContext == nil || actual.SecurityContext.ReadOnlyRootFilesystem == nil || !*actual.SecurityContext.ReadOnlyRootFilesystem {
			return nil, errors.New("artifact collector lacks its read-only isolated export contract")
		}
		return pod, nil
	}
	return nil, fmt.Errorf("artifact pod %s no longer has its observed identity", observed.Metadata.Name)
}

func sameArtifactContainers(before, after *Pod) bool {
	for _, name := range []string{ContainerStep, ContainerArtifacts} {
		a, b := podContainer(before, false, name), podContainer(after, false, name)
		if a == nil || b == nil || a.ContainerID != b.ContainerID || a.RestartCount != b.RestartCount || !reflect.DeepEqual(a.State, b.State) {
			return false
		}
	}
	return true
}

// Required artifacts cannot become a green step with a missing-file note.
func (s *step) collectStepArtifacts(ctx context.Context, pod *Pod, res *pl.StepResult, notes *noteList) {
	if len(s.run.Artifacts) == 0 {
		return
	}
	snapshot, err := s.r.collectArtifacts(ctx, s.run, pod, s.r.cfg.ArtifactMaxBytes)
	if err == nil {
		defer snapshot.Close()
		slices.SortFunc(snapshot.Files, func(a, b SnapshotFile) int { return strings.Compare(a.Path, b.Path) })
		files := s.files()
		for _, file := range snapshot.Files {
			var body []byte
			body, err = readSmallArtifact(file, s.r.cfg.ArtifactMaxBytes)
			if err != nil {
				break
			}
			fileID := files.store(ctx, artifactFileName(file.Path), artifactMIME(file.Path), body, "the artifact "+files.quote(file.Path), notes)
			if fileID == "" {
				err = errors.New("required artifact could not be durably filed")
				break
			}
			res.ArtifactFileIDs = append(res.ArtifactFileIDs, fileID)
		}
	}
	if err != nil {
		res.Status = pl.OutcomeFailed
		if res.Failure == nil {
			res.Failure = &pl.Failure{Code: pl.CodeArtifactUnavailable, Message: cutBytes(s.mask("required artifacts were not captured: "+err.Error()), failureMaxBytes)}
		}
	}
}

func sameCollectorSpec(a, b *Container) bool {
	return a.Image == b.Image && reflect.DeepEqual(a.Command, b.Command) && reflect.DeepEqual(a.Args, b.Args) && reflect.DeepEqual(a.Env, b.Env) && reflect.DeepEqual(a.VolumeMounts, b.VolumeMounts) && reflect.DeepEqual(a.SecurityContext, b.SecurityContext)
}
