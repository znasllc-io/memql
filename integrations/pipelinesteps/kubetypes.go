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

// ObjectMeta is the metadata every object carries. DeletionTimestamp is set
// by the API server the moment an object starts going away -- a delete, an
// eviction -- while a pod's containers still run out their grace period, and
// the kubelet's report of them still reads ready.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	UID               string            `json:"uid,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp,omitzero"`
	DeletionTimestamp time.Time         `json:"deletionTimestamp,omitzero"`
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
//
// The isolation probe (isolation.go) is the one Indexed Job: two pods of one
// Job, one per completion index. BackoffLimitPerIndex 0 means no index is ever
// retried and one that fails does not end the others -- the probe's connector
// ends failed while its listener has to keep running -- and it is a pointer
// because absent means something else again: no per-index limit at all.
type JobSpec struct {
	BackoffLimit            *int32          `json:"backoffLimit,omitempty"`
	ActiveDeadlineSeconds   *int64          `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32          `json:"ttlSecondsAfterFinished,omitempty"`
	CompletionMode          string          `json:"completionMode,omitempty"`
	Completions             *int32          `json:"completions,omitempty"`
	Parallelism             *int32          `json:"parallelism,omitempty"`
	BackoffLimitPerIndex    *int32          `json:"backoffLimitPerIndex,omitempty"`
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
	RestartPolicy                string `json:"restartPolicy,omitempty"`
	ServiceAccountName           string `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks           *bool  `json:"enableServiceLinks,omitempty"`
	// TerminationGracePeriodSeconds is a pointer because 0 means "kill at
	// once", while absent is the API's 30 seconds.
	TerminationGracePeriodSeconds *int64              `json:"terminationGracePeriodSeconds,omitempty"`
	SecurityContext               *PodSecurityContext `json:"securityContext,omitempty"`
	InitContainers                []Container         `json:"initContainers,omitempty"`
	Containers                    []Container         `json:"containers"`
	Volumes                       []Volume            `json:"volumes,omitempty"`
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
// a native sidecar -- a service, and the isolation probe's listener: "Always"
// on an init container is what makes it one, and on any other container it
// means something else or nothing at all.
// TerminationMessagePolicy "FallbackToLogsOnError" makes a failed container's
// terminated.message the tail of its log; with the default (File) it is empty.
type Container struct {
	Name                     string           `json:"name"`
	Image                    string           `json:"image,omitempty"`
	Command                  []string         `json:"command,omitempty"`
	Args                     []string         `json:"args,omitempty"`
	WorkingDir               string           `json:"workingDir,omitempty"`
	Env                      []EnvVar         `json:"env,omitempty"`
	VolumeMounts             []VolumeMount    `json:"volumeMounts,omitempty"`
	RestartPolicy            *string          `json:"restartPolicy,omitempty"`
	StartupProbe             *Probe           `json:"startupProbe,omitempty"`
	ReadinessProbe           *Probe           `json:"readinessProbe,omitempty"`
	Resources                *Resources       `json:"resources,omitempty"`
	SecurityContext          *SecurityContext `json:"securityContext,omitempty"`
	TerminationMessagePolicy string           `json:"terminationMessagePolicy,omitempty"`
}

// Resources are a container's requests and limits, as quantities. Only the
// isolation probe sets them: a step's containers take the LimitRange's
// defaults, which would ask a hundred times what a probe pod uses.
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// SecurityContext is a container's security context. RunAsUser is a pointer
// because 0 -- root -- is a value the runner writes, and absent means the
// image's own user.
type SecurityContext struct {
	RunAsUser                *int64        `json:"runAsUser,omitempty"`
	AllowPrivilegeEscalation *bool         `json:"allowPrivilegeEscalation,omitempty"`
	Capabilities             *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities are the Linux capabilities a container gives up from the
// container runtime's default set, by name ("NET_RAW") or all of them
// ("ALL"). Drop only: no container the runner builds adds one back.
type Capabilities struct {
	Drop []string `json:"drop,omitempty"`
}

// Probe is a service's startup probe -- an exec command polled until it
// succeeds -- or the isolation probe's readiness probe, a TCP connection the
// kubelet makes to the listener's port.
type Probe struct {
	Exec             *ExecAction      `json:"exec,omitempty"`
	TCPSocket        *TCPSocketAction `json:"tcpSocket,omitempty"`
	PeriodSeconds    int32            `json:"periodSeconds,omitempty"`
	FailureThreshold int32            `json:"failureThreshold,omitempty"`
}

// ExecAction is a command run inside the container.
type ExecAction struct {
	Command []string `json:"command,omitempty"`
}

// TCPSocketAction is a port the kubelet connects to; the probe passes when the
// connection is accepted.
type TCPSocketAction struct {
	Port int32 `json:"port"`
}

// EnvVar is one environment variable: a plain Value or a ValueFrom reference.
// An empty Value is omitted, which the API reads as the empty string -- the
// same thing.
type EnvVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource is where a referenced value comes from: one key of a Secret, or
// a field of the pod itself (the downward API).
type EnvVarSource struct {
	SecretKeyRef *SecretKeySelector   `json:"secretKeyRef,omitempty"`
	FieldRef     *ObjectFieldSelector `json:"fieldRef,omitempty"`
}

// ObjectFieldSelector names a field of the pod as the downward API spells it,
// e.g. metadata.labels['<key>'].
type ObjectFieldSelector struct {
	FieldPath string `json:"fieldPath"`
}

// SecretKeySelector is one key of one Secret. Optional is a pointer because
// true is written explicitly: the clone token's key is absent for an
// anonymous clone, and a required key that is absent stops the pod.
type SecretKeySelector struct {
	Name     string `json:"name"`
	Key      string `json:"key"`
	Optional *bool  `json:"optional,omitempty"`
}

// VolumeMount mounts a pod volume into a container; SubPath mounts one
// directory of it instead of its root.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
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

// PodStatus is a pod's observed state. Reason and Message are set when the
// pod as a whole failed or was stopped (Evicted, OutOfcpu, ...), which no
// container's state says. PodIP is the address the pod's sandbox was given:
// set once the sandbox exists, before any container has started, and what the
// isolation probe's connector dials.
type PodStatus struct {
	Phase                 string            `json:"phase,omitempty"`
	Reason                string            `json:"reason,omitempty"`
	Message               string            `json:"message,omitempty"`
	PodIP                 string            `json:"podIP,omitempty"`
	Conditions            []PodCondition    `json:"conditions,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
}

// PodCondition is one of a pod's conditions: PodScheduled says a step is
// waiting for room, and DisruptionTarget that the cluster is stopping the pod
// (a node drain, a preemption, an eviction). LastTransitionTime is when the
// condition took its status -- for DisruptionTarget, when the stopping began.
type PodCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime,omitzero"`
}

// ContainerStatus is one container's observed state. Image is the image the
// kubelet runs, or is trying to pull. RestartCount and LastState matter for a
// service: a sidecar is restarted whatever the pod's restart policy, so one
// that crashed, or that its startup probe killed, can read running again with
// only these two saying it stopped before. LastState is a value, and an empty
// one ({} on the wire) is the API saying there was no previous run. Ready is
// the container's readiness probe passing -- the isolation probe's listener
// accepting connections; false and absent alike are not ready.
type ContainerStatus struct {
	Name         string         `json:"name"`
	Image        string         `json:"image,omitempty"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState,omitzero"`
	RestartCount int32          `json:"restartCount,omitempty"`
	Ready        bool           `json:"ready,omitempty"`
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
