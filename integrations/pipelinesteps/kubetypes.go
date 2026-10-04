package pipelinesteps

import "time"

// The Kubernetes objects the runner sends and reads, as plain JSON structs.
//
// client-go, apimachinery and controller-runtime are banned from the module
// graph (TestNoKubernetesClientInTheModuleGraph), and the runner talks to the
// API server through component/deploycontrol's ClusterAPI, so these are the
// whole of its Kubernetes vocabulary. Two rules keep them honest:
//
//   - Every JSON name is the API's own, letter for letter. A misspelt key is
//     not an error anywhere: the API server drops an unknown field from a
//     create and the decoder drops it from a read, so the Job silently loses
//     a setting or the classifier silently reads a zero.
//   - Only the fields the code writes or reads. A field nothing sets is not
//     declared, so nothing here can be mistaken for a setting the Job has.
//
// Where a ZERO means something different from ABSENT, the field is a pointer
// or a value with omitempty chosen accordingly: backoffLimit 0 is "never
// retry" while an absent one is six retries, automountServiceAccountToken
// false is "no token" while an absent one mounts it. Timestamps use omitzero,
// because the API writes a missing one as null and time.Time reads null as
// the zero time.

// ObjectMeta is the metadata every object carries.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	UID               string            `json:"uid,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp,omitzero"`
}

// OwnerReference makes an object garbage-collected with its owner: the step's
// Secret is owned by its Job once the Job exists. Controller and
// BlockOwnerDeletion are pointers because the runner writes them false.
type OwnerReference struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         *bool  `json:"controller,omitempty"`
	BlockOwnerDeletion *bool  `json:"blockOwnerDeletion,omitempty"`
}

// Job is a batch/v1 Job.
type Job struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       JobSpec    `json:"spec"`
	Status     JobStatus  `json:"status,omitzero"`
}

// JobSpec holds the three numbers that decide a step's lifecycle. Each is a
// pointer because its zero is meaningful: backoffLimit 0 never retries,
// ttlSecondsAfterFinished 0 deletes the Job the moment it ends.
type JobSpec struct {
	BackoffLimit            *int32          `json:"backoffLimit,omitempty"`
	ActiveDeadlineSeconds   *int64          `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32          `json:"ttlSecondsAfterFinished,omitempty"`
	Template                PodTemplateSpec `json:"template"`
}

// JobStatus is what the classifier reads off a Job.
type JobStatus struct {
	Conditions     []JobCondition `json:"conditions,omitempty"`
	StartTime      time.Time      `json:"startTime,omitzero"`
	CompletionTime time.Time      `json:"completionTime,omitzero"`
	Active         int32          `json:"active,omitempty"`
	Succeeded      int32          `json:"succeeded,omitempty"`
	Failed         int32          `json:"failed,omitempty"`
}

// JobCondition is one of a Job's conditions: Complete or Failed, with the
// reason (DeadlineExceeded is the one that matters).
type JobCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// PodTemplateSpec is the pod a Job creates.
type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata,omitzero"`
	Spec     PodSpec    `json:"spec"`
}

// PodSpec is the step's pod.
type PodSpec struct {
	RestartPolicy                string              `json:"restartPolicy,omitempty"`
	ServiceAccountName           string              `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken *bool               `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks           *bool               `json:"enableServiceLinks,omitempty"`
	SecurityContext              *PodSecurityContext `json:"securityContext,omitempty"`
	InitContainers               []Container         `json:"initContainers,omitempty"`
	Containers                   []Container         `json:"containers"`
	Volumes                      []Volume            `json:"volumes,omitempty"`
}

// PodSecurityContext is the pod-wide security context.
type PodSecurityContext struct {
	SeccompProfile *SeccompProfile `json:"seccompProfile,omitempty"`
}

// SeccompProfile names a seccomp profile; the step pod uses RuntimeDefault.
type SeccompProfile struct {
	Type string `json:"type"`
}

// Container is one container of the step's pod. RestartPolicy is set only on
// a service: "Always" on an init container is what makes it a native sidecar,
// and on any other container it means something else or nothing at all.
type Container struct {
	Name            string           `json:"name"`
	Image           string           `json:"image,omitempty"`
	Command         []string         `json:"command,omitempty"`
	Args            []string         `json:"args,omitempty"`
	WorkingDir      string           `json:"workingDir,omitempty"`
	Env             []EnvVar         `json:"env,omitempty"`
	VolumeMounts    []VolumeMount    `json:"volumeMounts,omitempty"`
	RestartPolicy   *string          `json:"restartPolicy,omitempty"`
	StartupProbe    *Probe           `json:"startupProbe,omitempty"`
	SecurityContext *SecurityContext `json:"securityContext,omitempty"`
}

// SecurityContext is a container's security context.
type SecurityContext struct {
	AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation,omitempty"`
}

// Probe is a startup probe: an exec command polled until it succeeds.
type Probe struct {
	Exec             *ExecAction `json:"exec,omitempty"`
	PeriodSeconds    int32       `json:"periodSeconds,omitempty"`
	FailureThreshold int32       `json:"failureThreshold,omitempty"`
}

// ExecAction is a command run inside the container.
type ExecAction struct {
	Command []string `json:"command,omitempty"`
}

// EnvVar is one environment variable: a plain Value or a ValueFrom reference.
// An empty Value is omitted, which the API reads as the empty string -- the
// same thing.
type EnvVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource is where a referenced value comes from.
type EnvVarSource struct {
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty"`
}

// SecretKeySelector is one key of one Secret. Optional is a pointer because
// true is written explicitly: the clone token's key is absent for an
// anonymous clone, and a required key that is absent stops the pod.
type SecretKeySelector struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Optional *bool  `json:"optional,omitempty"`
}

// VolumeMount mounts a pod volume into a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

// Volume is one pod volume: scratch (EmptyDir) or a claim.
type Volume struct {
	Name                  string                             `json:"name"`
	EmptyDir              *EmptyDirVolumeSource              `json:"emptyDir,omitempty"`
	PersistentVolumeClaim *PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaim,omitempty"`
}

// EmptyDirVolumeSource marshals as {}: present, with every setting defaulted.
type EmptyDirVolumeSource struct{}

// PersistentVolumeClaimVolumeSource mounts a claim by name.
type PersistentVolumeClaimVolumeSource struct {
	ClaimName string `json:"claimName"`
}

// Secret is a core/v1 Secret. Data values are bytes, which encoding/json
// writes as base64 -- the API's own encoding of a Secret's data.
type Secret struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
}

// Pod is what the runner reads to classify a step: its phase and the state of
// each container.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   PodStatus  `json:"status"`
}

// PodList is a list response; the runner finds a Job's pod by label.
type PodList struct {
	Items []Pod `json:"items"`
}

// PodStatus is a pod's observed state.
type PodStatus struct {
	Phase                 string            `json:"phase,omitempty"`
	Conditions            []PodCondition    `json:"conditions,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
}

// PodCondition is one of a pod's conditions (PodScheduled is the one that
// says a step is waiting for room).
type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerStatus is one container's observed state.
type ContainerStatus struct {
	Name  string         `json:"name"`
	State ContainerState `json:"state"`
}

// ContainerState is exactly one of waiting, running or terminated.
type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting,omitempty"`
	Running    *ContainerStateRunning    `json:"running,omitempty"`
	Terminated *ContainerStateTerminated `json:"terminated,omitempty"`
}

// ContainerStateWaiting carries the reason a container has not started
// (ImagePullBackOff, CreateContainerConfigError, ...).
type ContainerStateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerStateRunning says since when a container runs.
type ContainerStateRunning struct {
	StartedAt time.Time `json:"startedAt,omitzero"`
}

// ContainerStateTerminated is how a container ended. ExitCode has no
// omitempty: a 0 is the answer, not an absence.
type ContainerStateTerminated struct {
	ExitCode   int32     `json:"exitCode"`
	Reason     string    `json:"reason,omitempty"`
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}
