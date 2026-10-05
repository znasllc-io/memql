package pipelinesteps

import (
	"os"
	"strconv"
	"strings"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Config is everything the runner and the executor read from the node's
// environment, plus the constants they share. The environment knobs are
// registered in scripts/secrets/manifest.yaml (component pipelines); the
// pipelines component's ConfigMap supplies the namespace and the clone image
// to the workbench node.
type Config struct {
	// Namespace is where step Jobs and their Secrets live
	// (MEMQL_PIPELINES_NAMESPACE, default memql-pipelines). It must be the
	// namespace whose Role grants the engine identity Jobs.
	Namespace string
	// CloneImage runs the clone init container (MEMQL_PIPELINES_CLONE_IMAGE).
	// There is no default: an image nobody chose would be a guess about what
	// may run with a repository token, so an empty value means this node
	// cannot run pipeline steps, and the runner refuses them.
	CloneImage string
	// CacheClaim is the PersistentVolumeClaim mounted at /cache.
	CacheClaim string
	// StepServiceAccount is the identity a step's pod runs as: one holding no
	// RoleBinding, whose token is never mounted.
	StepServiceAccount string
	// RunCeiling bounds a whole run's wall-clock time
	// (MEMQL_PIPELINES_RUN_MAX_MINUTES, default 120, clamped to 5..1440), as
	// the seam parses it (pl.ParseRunCeiling): the driver waits on a run's
	// steps by the same reading.
	RunCeiling time.Duration
	// LogStoreMaxLines is how many lines of one step reach the log store
	// (MEMQL_PIPELINES_LOG_STORE_MAX_LINES, default 2000, clamped to
	// 100..100000); the Library archive keeps the rest.
	LogStoreMaxLines int
	// ArtifactMaxBytes caps one step's decoded artifact archive
	// (MEMQL_PIPELINES_ARTIFACT_MAX_BYTES, default 64 MiB, clamped to
	// 1 MiB..256 MiB).
	ArtifactMaxBytes int64
	// ArchiveMaxBytes caps one step's archived log.
	ArchiveMaxBytes int64
	// DefaultStepTimeout applies to a step that declares none (D11).
	DefaultStepTimeout time.Duration
	// JobTTL is the backstop that collects a finished Job and, through its
	// owner reference, its Secret. The ack deletes both sooner.
	JobTTL time.Duration
	// ScheduleTimeout is how long a pod may stay unschedulable.
	ScheduleTimeout time.Duration
	// PollInterval is how often the runner reads its Job.
	PollInterval time.Duration
	// HeartbeatInterval is how often the runner re-stamps AnnotRunner.
	HeartbeatInterval time.Duration
	// HeartbeatStale is how old a heartbeat may be before another replica
	// may adopt the Job.
	HeartbeatStale time.Duration
	// NodeID is THIS node's id, as the mesh knows it.
	NodeID string
	// IsolationTTL is how long this replica trusts a passed isolation proof
	// (isolation.go) before it proves again: an hour. A proof that did not
	// pass is never trusted, so the next create proves again. Not an
	// environment knob.
	IsolationTTL time.Duration
	// WorkspaceLimit is the sizeLimit of every step's workspace, a quantity
	// (MEMQL_PIPELINES_WORKSPACE_LIMIT, default 20Gi): the pipelines
	// LimitRange's default ephemeral-storage limit, which the component's
	// ConfigMap states beside it, so the bound the kubelet enforces on the
	// pod is the one the Job shows on its workspace.
	WorkspaceLimit string
}

// The environment this package reads. Named once, so the env-registry scan
// resolves every read to its key.
const (
	envNamespace        = "MEMQL_PIPELINES_NAMESPACE"
	envCloneImage       = "MEMQL_PIPELINES_CLONE_IMAGE"
	envLogStoreMaxLines = "MEMQL_PIPELINES_LOG_STORE_MAX_LINES"
	envArtifactMaxBytes = "MEMQL_PIPELINES_ARTIFACT_MAX_BYTES"
	envNodeID           = "MEMQL_NODE_ID"
	envWorkspaceLimit   = "MEMQL_PIPELINES_WORKSPACE_LIMIT"
)

const (
	defaultNamespace   = "memql-pipelines"
	cacheClaim         = "memql-pipelines-cache"
	stepServiceAccount = "memql-pipelines-step"
	mebibyte           = 1 << 20
	// workspaceLimit is the component's default ephemeral-storage limit,
	// which every overlay restates beside its LimitRange.
	workspaceLimit = "20Gi"
)

// ConfigFromEnv reads the configuration through getenv (os.Getenv when nil).
//
// A knob that is set but is not a positive whole number falls back to its
// DEFAULT, never to a bound and never to "no limit": zero minutes is not a
// request for the shortest ceiling, and an unbounded run is the one outcome a
// misconfigured cap must not produce. A value past a bound is clamped to it.
func ConfigFromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	text := func(key string) string { return strings.TrimSpace(getenv(key)) }
	whole := func(key string, def, lo, hi int) int { return clampedWhole(getenv(key), def, lo, hi) }

	namespace := text(envNamespace)
	if namespace == "" {
		namespace = defaultNamespace
	}
	return Config{
		Namespace:          namespace,
		CloneImage:         text(envCloneImage),
		CacheClaim:         cacheClaim,
		StepServiceAccount: stepServiceAccount,
		RunCeiling:         pl.ParseRunCeiling(text(pl.EnvRunCeiling)),
		LogStoreMaxLines:   whole(envLogStoreMaxLines, 2000, 100, 100000),
		ArtifactMaxBytes:   int64(whole(envArtifactMaxBytes, 64*mebibyte, mebibyte, 256*mebibyte)),
		ArchiveMaxBytes:    64 * mebibyte,
		DefaultStepTimeout: 20 * time.Minute,
		JobTTL:             30 * time.Minute,
		ScheduleTimeout:    10 * time.Minute,
		PollInterval:       2 * time.Second,
		HeartbeatInterval:  10 * time.Second,
		HeartbeatStale:     45 * time.Second,
		IsolationTTL:       time.Hour,
		NodeID:             nodeID(text(envNodeID)),
		WorkspaceLimit:     workspaceLimitOf(text(envWorkspaceLimit)),
	}
}

// workspaceLimitOf is raw as a workspace size limit, or the default. It goes
// into every step's Job as written, so it must be a quantity the API server
// takes and one an operator means: a positive whole number of Ki, Mi, Gi or
// Ti. A bare number is bytes, and a workspace of 20 bytes evicts every step.
func workspaceLimitOf(raw string) string {
	if isWorkspaceLimit(raw) {
		return raw
	}
	return workspaceLimit
}

// isWorkspaceLimit reports whether s is a positive whole number of Ki, Mi, Gi
// or Ti, written plainly: no sign, no leading zero, no space.
func isWorkspaceLimit(s string) bool {
	for _, unit := range []string{"Ki", "Mi", "Gi", "Ti"} {
		if digits, ok := strings.CutSuffix(s, unit); ok {
			n, err := strconv.Atoi(digits)
			return err == nil && n > 0 && digits == strconv.Itoa(n)
		}
	}
	return false
}

// clampedWhole reads raw as a positive whole number within lo..hi. Blank,
// unparseable and non-positive are def; past a bound is the bound.
func clampedWhole(raw string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	switch {
	case err != nil || n <= 0:
		return def
	case n < lo:
		return lo
	case n > hi:
		return hi
	}
	return n
}

// nodeID is THIS node's id by the derivation component/node.NewIdentity uses
// and integrations/workbench's selfNodeId repeats: MEMQL_NODE_ID, trimmed,
// else the hostname. It has to AGREE with that derivation, because the value
// the runner stamps on a Job's heartbeat and on Where.NodeID is compared with
// PeerInfo.node_id, which is Identity.ID; TestNodeIDAgreesWithTheNodeIdentity
// holds the two together. Repeated rather than imported for selfNodeId's
// reason: building a whole Identity to ask one question would read a dozen
// other variables as a side effect.
func nodeID(fromEnv string) string {
	if fromEnv != "" {
		return fromEnv
	}
	if host, err := os.Hostname(); err == nil {
		return strings.TrimSpace(host)
	}
	return ""
}
