package pipelinesteps

import (
	"regexp"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// Every name the runner gives an object is DERIVED, never stored: the replica
// that creates a Job, a replica adopting it after the first one went quiet,
// and the agent asking for its status each compute the same name from the
// same inputs. That is what lets a step survive the replica running it. It is
// also why the derivations are pinned by literal values in names_test.go:
// during a rolling deploy an old replica and a new one must still agree.
//
// The derivations are core/id content addresses, as every derived id under
// integrations/ is (TestNoSHA256InIntegrations): deterministic, independent
// of any setting or salt, and over a keyed map, so no field can run into the
// next the way a joined string's parts can. The engine is untracked because
// the runner derives names for as long as it lives.
var namesEngine = id.NewUntracked()

// JobName is deterministic per (run, step, attempt): a retried step gets a
// fresh Job, a resumed driver re-attaches to the same one. "mp-" plus the
// first 24 hex of the content address of the three -- 27 characters, a DNS
// label, so the Job controller can copy it into the job-name label every pod
// of the Job carries.
func JobName(runID, stepKey string, attempt int) string {
	sum := namesEngine.MustFromMap(map[string]any{"runId": runID, "stepKey": stepKey, "attempt": attempt})
	return "mp-" + string(sum)[:24]
}

// SecretName is the name of the Secret that carries a Job's clone token and
// resolved secrets.
func SecretName(jobName string) string { return jobName + "-env" }

// ArtifactMarker is the token the step wrapper frames artifacts with: it
// prints the lines "<marker> begin" and "<marker> end", each a whole line of
// its own (the begin line after a newline of its own, so a command that ended
// without one cannot glue its last words to it), and the capture matches them
// as whole lines. It is derived from the Job's name alone, so whichever
// replica captures the output knows it without asking the one that built the
// Job.
//
// It is not a secret, and does not need to be: the step can read it from its
// own environment, and a step that forges a frame gains nothing it could not
// get by declaring the artifact. What it buys is that ordinary output never
// collides with the frame by accident.
func ArtifactMarker(jobName string) string {
	sum := namesEngine.MustFromMap(map[string]any{"artifactsOf": jobName})
	return "::memql-artifacts::" + string(sum)[:16]
}

// The labels every object the runner creates carries, and the annotations it
// reads back. LabelRun is how a cancel selects a whole run.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelRun       = "memql.io/pipelines-run"
	LabelAttempt   = "memql.io/attempt"

	AnnotStepKey = "memql.io/step-key"
	AnnotWorkRun = "memql.io/work-run"
	AnnotOwner   = "memql.io/owner-user"
	// AnnotRunner is "<nodeId> <RFC3339 heartbeat>": which replica holds the
	// Job, and when it last said so.
	AnnotRunner = "memql.io/runner"
	// AnnotLogCursor is the RFC3339Nano timestamp of the last line captured,
	// where an adopting replica resumes the log.
	AnnotLogCursor = "memql.io/log-cursor"
	// AnnotLogFirst is the RFC3339Nano timestamp of the step's first line, as
	// the runner that followed it from its start captured it; written once,
	// and never by an adopter. An adopter compares it with the first line the
	// node's log still serves to tell whether its archive has the step's head
	// (the kubelet serves only the container's current log file).
	AnnotLogFirst = "memql.io/log-first"
	// AnnotOutcome is the pl.StepResult as JSON, written before the reply is
	// sent, so a lost reply can be answered again without re-running.
	AnnotOutcome = "memql.io/outcome"
	// AnnotObservation is how the step ended, as the runner FIRST saw it --
	// a pl.StepResult holding only the status, the exit code, the failure and
	// the times -- recorded the moment it is seen, before the slow half of
	// settling the step (its log and artifacts into the Library). An adopter
	// settles the step by it rather than looking again: "finished inside its
	// deadline" lasts only as long as the pod does, and once the Job
	// controller has deleted a pod at the deadline the same Job reads
	// DeadlineExceeded.
	AnnotObservation = "memql.io/observation"
)

// ManagedBy is LabelManagedBy's value on everything the runner creates.
const ManagedBy = "memql-workbench"

// LabelProbe marks the isolation probe's Job, its pods and its Secret, with
// ProbeIsolation. They carry no run label, so nothing that selects a run's
// objects -- a cancel's deletecollection -- reaches them.
const (
	LabelProbe     = "memql.io/probe"
	ProbeIsolation = "isolation"
)

// The containers of an isolation probe pod (jobspec.go): the listener, a
// native sidecar, and the connector, which on index 0 only holds its pod up.
const (
	ContainerProbeListener  = "listener"
	ContainerProbeConnector = "connector"
)

// completionIndexKey is the annotation (and, since Kubernetes 1.28, the
// label) the Job controller puts on each pod of an Indexed Job, naming its
// index; controllerUIDLabel is the label naming the pod's Job by uid
// (Kubernetes 1.27 and later).
const (
	completionIndexKey = "batch.kubernetes.io/job-completion-index"
	controllerUIDLabel = "batch.kubernetes.io/controller-uid"
)

// The containers of a step's pod, by name. ServicePrefix + a service's name is
// that service's sidecar. ContainerCachePrep runs first, and only when the
// step declares caches; its failure is a Job the runner could not set up
// (pl.CodeJobRejected), not a clone failure.
const (
	ContainerCachePrep = "cache-prep"
	ContainerClone     = "clone"
	ContainerStep      = "step"
	ServicePrefix      = "svc-"
)

// labelValueShape is a Kubernetes label value: alphanumeric at both ends,
// with dashes, underscores and dots between.
var labelValueShape = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// RunLabelValue is the value of LabelRun for a run. A bare id is already a
// legal label value and is kept as it is, so `kubectl get jobs -l` reads the
// id an operator has; anything else (longer than 63 characters, a canonical id
// with colons) is replaced by "r-" plus 24 hex of its content address. Every
// selector a cancel builds goes through this same function, so it matches the
// label.
func RunLabelValue(runID string) string {
	if len(runID) <= 63 && labelValueShape.MatchString(runID) {
		return runID
	}
	sum := namesEngine.MustFromMap(map[string]any{"runId": runID})
	return "r-" + string(sum)[:24]
}

// IsolationProbeName is the Job of this replica's isolation probe
// (isolation.go): "mpi-" plus 24 hex of the content address of the node id.
// One name per replica, the same every time, so a replica's next proof finds
// and removes whatever its last one left -- and never another replica's,
// whose proof may be running. It is never the shape of a step's Job
// (stepJobNameShape), so neither Status nor Ack can address it.
func IsolationProbeName(nodeID string) string {
	sum := namesEngine.MustFromMap(map[string]any{"isolationProbeOf": nodeID})
	return "mpi-" + string(sum)[:24]
}

// IsolationTargetName is the probe's Secret, which hands the connector the
// listener's address. It does not end in SecretName's suffix, so the
// orphan-Secret sweep never judges it; its owner reference collects it with
// the probe Job.
func IsolationTargetName(probeName string) string { return probeName + "-target" }

// CacheTrust is the half of a repository's cache a run reads and writes
// (ruling R15b): CacheTrusted for what the repository has accepted,
// CacheUntrusted for what a collaborator proposes.
type CacheTrust string

const (
	CacheTrusted   CacheTrust = "trusted"
	CacheUntrusted CacheTrust = "untrusted"
)

// CacheSubPath is a step's directory of the cache claim (rulings R15, R15b):
// "owners/<owner>/repos/<repository>/<trust>", the owner 24 hex of the content
// address of the owner's id, the repository 24 hex of that of its owner and
// name, and the trust CacheTrusted or CacheUntrusted -- any other value is
// CacheUntrusted. The step mounts this directory as /cache, never the claim's
// root.
//
// A step can rewrite any entry of the cache it is given, and Go trusts its
// build cache by action id, so whoever writes a cache decides what a later
// build that reads it gets. Hence three walls: two owners never share a cache;
// two repositories of one owner never do; and a pull request's run never
// writes the cache a run of the default branch, the merge queue or a release
// reads (cacheTrustOf). Hashed, so neither an owner id nor a repository name
// can name a path of its own choosing, and over a keyed map, so an owner and
// a name can never run together into another repository's; derived, so every
// replica and every version mounts the same directory.
func CacheSubPath(ownerUserID string, repository pl.Repository, trust CacheTrust) string {
	if trust != CacheTrusted {
		trust = CacheUntrusted
	}
	owner := namesEngine.MustFromMap(map[string]any{"ownerUserId": ownerUserID})
	repo := namesEngine.MustFromMap(map[string]any{"repositoryOwner": repository.Owner, "repositoryName": repository.Name})
	return "owners/" + string(owner)[:24] + "/repos/" + string(repo)[:24] + "/" + string(trust)
}

// eventVar is the contract variable pl.StepRequest.Environment renders the
// run's event in, which reaches the runner as a plain value of StepRun.Env. No
// step can set it: the seam keeps the platform's value over a secret of the
// same name, the compiler and BuildJob refuse a secret named MEMQL_*, and a
// manifest declares no environment of its own.
const eventVar = "MEMQL_EVENT"

// cacheTrustOf is the trust of a step's cache, by the event its run was opened
// for (ruling R15b): a push to the default branch, the merge queue and a
// published release build what the repository has accepted; a pull request
// builds what a collaborator proposes. A re-run is opened with its original's
// event, so it keeps its original's trust. An event this version does not
// know, or none, is untrusted: a run the runner cannot place never writes the
// cache the default branch reads.
func cacheTrustOf(run StepRun) CacheTrust {
	switch pl.Event(run.Env[eventVar]) {
	case pl.EventPush, pl.EventMergeGroup, pl.EventRelease:
		return CacheTrusted
	}
	return CacheUntrusted
}
