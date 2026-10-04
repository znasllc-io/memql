package pipelinesteps

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// A step's pod, end to end (design record D10, rulings R13b, R15b, R17):
//
//	init  cache-prep     only when caches are declared: root, the claim's ROOT
//	                     at /cache-root, opens the step's directory under it
//	                     (CacheSubPath) -- no git, no token, no checkout
//	init  clone          cfg.CloneImage as root: shallow-fetches the SHA into
//	                     /workspace, the ONLY container that can read the clone
//	                     token; never sees the cache claim
//	init  svc-<name>...  one native sidecar per service, sorted by name; the
//	                     kubelet starts the next container only once a
//	                     sidecar's startup probe passes
//	main  step           the manifest's image as its own user, running the
//	                     command through stepWrapper in /workspace, with the
//	                     contract environment, the step's secrets by reference,
//	                     and only its own directory of the cache at /cache: its
//	                     owner's, its repository's and its run's trust's
//
// No cluster credential of any kind is present: the pod runs as an identity
// with no RoleBinding, its token is never mounted, Service links are off, and
// the only Secret it can name is its own. No container may gain privileges,
// and none keeps the runtime's NET_RAW (rootContext, imageContext).
//
// BuildJob and BuildSecret are pure: the same inputs give the same objects,
// which is what lets a replica rebuild what another replica created.

const (
	workspaceVolume = "workspace"
	workspacePath   = "/workspace"
	cacheVolume     = "cache"
	cachePath       = "/cache"

	// cacheRootPath is where cache-prep mounts the cache claim's ROOT, and
	// cacheDirVar names the step's directory under it (CacheSubPath) that
	// cache-prep opens, world-writable and sticky (rulings R15, R15b). Kubelet
	// creates a missing subPath root-owned with the claim root's mode, so on a
	// claim whose root is not world-writable a non-root step would get a cache
	// it cannot enter; the sticky bit keeps one uid of the steps that share the
	// directory from renaming or replacing another uid's tree. Only cache-prep
	// -- a fixed script, no git, no token -- ever sees the claim's root, where
	// every owner's caches live.
	cacheRootPath = "/cache-root"
	cacheDirVar   = "CACHE_DIR"

	// failureFromLogs is the termination message policy of cache-prep, the
	// clone and every sidecar: with the default (File) a failed container's
	// terminated.message is empty, and the runner's classifier quotes it to say
	// why the step never ran. The step keeps the default; its own output is
	// captured from its log stream.
	failureFromLogs = "FallbackToLogsOnError"

	// gitTokenKey is the clone token's key in the step's Secret, and the
	// variable only the clone container receives it as.
	gitTokenKey = "GIT_TOKEN"

	// The wrapper's own variables (wrapper.go). stepCachesVar names the
	// declared caches; the wrapper derives each uid's cache paths from it.
	stepCommandVar    = "MEMQL_STEP_COMMAND"
	stepArtifactsVar  = "MEMQL_STEP_ARTIFACTS"
	artifactMarkerVar = "MEMQL_ARTIFACT_MARKER"
	stepCachesVar     = "MEMQL_CACHES"

	// The step's git configuration (ruling R13). The clone container's uid
	// never matches every step image's, and git refuses a repository another
	// user owns -- root included -- so the step names /workspace safe. Through
	// git's environment configuration because that is the command scope, one
	// of the protected scopes safe.directory is read from; a repository's own
	// config cannot vouch for itself. A substrate implementation detail, like
	// the wrapper's per-uid cache paths, not part of the MEMQL_* contract.
	gitConfigCountVar = "GIT_CONFIG_COUNT"
	gitConfigKeyVar   = "GIT_CONFIG_KEY_0"
	gitConfigValueVar = "GIT_CONFIG_VALUE_0"

	// platformPrefix is the platform's namespace of variables. A step secret
	// may not be named in it: the seam's compiler already refuses one, and
	// pl.StepRequest.Environment drops one, so this is the third wall.
	platformPrefix = "MEMQL_"

	// artifactForbidden are characters an artifact path may not hold beyond
	// whitespace. The wrapper word-splits the list unquoted on purpose (globs
	// expand), and none of these has any business in a path it hands to tar.
	artifactForbidden = ";&|$\\`'\"<>"

	// stepGracePeriodSeconds bounds how long a cancelled step's pod lingers
	// (ruling R17): the wrapper is PID 1 and does not forward SIGTERM, so the
	// API's default 30 seconds would be 30 seconds of a step nobody wants.
	stepGracePeriodSeconds = 10
)

// cacheVariables is the closed set of caches the runner knows, and the
// variables the wrapper points into the step's own uid tree of the owner's
// cache when that cache is declared. Each is reserved while its cache is
// declared: the wrapper would overwrite a step secret of the same name as the
// step starts.
var cacheVariables = map[string][]string{
	"go":  {"GOMODCACHE", "GOCACHE"},
	"npm": {"npm_config_cache"},
}

var (
	// envNameShape is a portable shell variable name: what the step's command
	// reads as $NAME, and a legal key in a Secret. The step's own variables --
	// the contract and its secrets -- are held to it.
	envNameShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// serviceEnvNameShape is the API's relaxed rule for a variable name, its
	// regular expression only: the API's IsEnvVarName additionally refuses ".",
	// ".." and names starting "..", which reach the API and come back as a
	// rejected Job. A service's environment is read by its image's own process
	// rather than a shell, so dotted names its image documents
	// (discovery.type) are legal.
	serviceEnvNameShape = regexp.MustCompile(`^[-._a-zA-Z][-._a-zA-Z0-9]*$`)
	// shaShape is a full object id, SHA-1 or SHA-256. Never an abbreviation
	// (ambiguous) and never a ref name (it moves).
	shaShape = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	// dnsLabelShape is an RFC 1123 label, what a container name must be.
	dnsLabelShape = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// BuildSecret holds the clone token (key GIT_TOKEN, read ONLY by the clone
// container) and each resolved secret (key = its env name, referenced by the
// step container one secretKeyRef at a time). It is created before the Job
// and owned by it once the Job exists, so collecting the Job collects it.
//
// Build the Job FIRST: BuildJob is where a run is refused, and a refused run
// must leave nothing in the namespace. BuildSecret itself cannot refuse, so it
// is defensive on its own account: a step secret named GIT_TOKEN never lands
// in the Secret, whatever the clone token is. Were it written, an anonymous
// clone would send that secret to the repository's host as a credential.
func BuildSecret(cfg Config, run StepRun, jobName, cloneToken string) Secret {
	data := make(map[string][]byte, len(run.Secrets)+1)
	for name, value := range run.Secrets {
		if name == gitTokenKey {
			continue
		}
		data[name] = []byte(value)
	}
	if cloneToken != "" {
		data[gitTokenKey] = []byte(cloneToken)
	}
	if len(data) == 0 {
		data = nil
	}
	return Secret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: ObjectMeta{
			Name:        SecretName(jobName),
			Namespace:   cfg.Namespace,
			Labels:      objectLabels(run),
			Annotations: objectAnnotations(run),
		},
		Type: "Opaque",
		Data: data,
	}
}

// BuildJob is the step's batch/v1 Job, a pure function of its inputs. It
// refuses -- with a *pl.Refusal and a zero Job, never a half-built one -- any
// input it cannot turn into a Job that is both correct and safe: no image or
// command, no time left, an artifact path that can name anything outside the
// working copy or carries shell metacharacters, a variable or secret name the
// step cannot have, a secret that collides with a name the platform sets, an
// unknown cache, and the malformed identities (owner, run id, attempt, SHA,
// clone URL, service) the API server or git would otherwise reject later and
// less legibly.
func BuildJob(cfg Config, run StepRun, jobName string) (Job, error) {
	ttl, refusal := checkJob(cfg, run)
	if refusal != nil {
		return Job{}, refusal
	}
	secretName := SecretName(jobName)
	caches := declaredCaches(run.Caches)

	var initContainers []Container
	if len(caches) > 0 {
		initContainers = append(initContainers, cachePrepContainer(cfg, run))
	}
	initContainers = append(initContainers, cloneContainer(cfg, run, secretName))
	initContainers = append(initContainers, serviceContainers(run.Services)...)

	volumes := []Volume{{Name: workspaceVolume, EmptyDir: &EmptyDirVolumeSource{}}}
	if len(caches) > 0 {
		volumes = append(volumes, Volume{
			Name:                  cacheVolume,
			PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: cfg.CacheClaim},
		})
	}

	return Job{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: ObjectMeta{
			Name:        jobName,
			Namespace:   cfg.Namespace,
			Labels:      objectLabels(run),
			Annotations: objectAnnotations(run),
		},
		Spec: JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(run.TimeoutSeconds)),
			TTLSecondsAfterFinished: ptr(ttl),
			Template: PodTemplateSpec{
				Metadata: ObjectMeta{Labels: objectLabels(run)},
				Spec: PodSpec{
					RestartPolicy:                 "Never",
					ServiceAccountName:            cfg.StepServiceAccount,
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					TerminationGracePeriodSeconds: ptr(int64(stepGracePeriodSeconds)),
					SecurityContext: &PodSecurityContext{
						SeccompProfile: &SeccompProfile{Type: "RuntimeDefault"},
					},
					InitContainers: initContainers,
					Containers:     []Container{stepContainer(run, jobName, secretName, caches)},
					Volumes:        volumes,
				},
			},
		},
	}, nil
}

// checkJob is every refusal, in one place, before anything is built. It
// answers the Job's TTL in seconds, which it has to range-check anyway.
func checkJob(cfg Config, run StepRun) (int32, *pl.Refusal) {
	scope := run.StepKey
	refuse := func(format string, args ...any) (int32, *pl.Refusal) {
		return 0, pl.Refuse(pl.CodeJobRejected, scope, format, args...)
	}

	if strings.TrimSpace(cfg.CloneImage) == "" {
		return 0, pl.Refuse(pl.CodeRunnerUnavailable, scope,
			"this workbench node has no clone image configured (MEMQL_PIPELINES_CLONE_IMAGE), so it cannot run pipeline steps")
	}
	ttl := int64(cfg.JobTTL / time.Second)
	if ttl <= 0 || ttl > math.MaxInt32 {
		return refuse("the Job TTL %v is not a positive number of seconds that fits the API: a zero TTL deletes the Job before its outcome is read", cfg.JobTTL)
	}
	if strings.TrimSpace(run.OwnerUserID) == "" {
		return refuse("the step has no owner: its cache, its Library files and its secrets are all an owner's")
	}
	if strings.TrimSpace(run.RunID) == "" {
		return refuse("the step has no run id, so nothing could select its Job for a cancel")
	}
	if run.Attempt < 1 {
		return refuse("attempt %d is not a 1-based attempt", run.Attempt)
	}
	if run.TimeoutSeconds <= 0 {
		return refuse("the step has %d seconds left to run; a Job without a deadline would run until its TTL", run.TimeoutSeconds)
	}
	if strings.TrimSpace(run.Image) == "" {
		return refuse("the step names no image to run in")
	}
	if strings.TrimSpace(run.Command) == "" {
		return refuse("the step has no command to run")
	}
	if !shaShape.MatchString(run.SHA) {
		return refuse("%q is not a full commit SHA; the clone fetches exactly one commit by its id", run.SHA)
	}
	if problem := cloneURLProblem(run.Repository.CloneURL); problem != "" {
		return refuse("%s", problem)
	}
	for _, name := range run.Caches {
		if _, known := cacheVariables[name]; !known {
			return refuse("cache %q is not one the runner knows (%s)", name, strings.Join(sortedKeys(cacheVariables), ", "))
		}
	}
	for _, path := range run.Artifacts {
		if reason := artifactProblem(path); reason != "" {
			return refuse("artifact path %q %s", path, reason)
		}
	}
	for _, name := range sortedKeys(run.Services) {
		if container := ServicePrefix + name; len(container) > 63 || !dnsLabelShape.MatchString(container) {
			return refuse("service %q does not make a container name: lowercase letters, digits and dashes, at most %d characters", name, 63-len(ServicePrefix))
		}
		if strings.TrimSpace(run.Services[name].Image) == "" {
			return refuse("service %q names no image", name)
		}
		for _, variable := range sortedKeys(run.Services[name].Env) {
			if !serviceEnvNameShape.MatchString(variable) {
				return refuse("service %q variable %q is not a name Kubernetes accepts for an environment variable", name, variable)
			}
		}
	}
	for _, name := range sortedKeys(run.Env) {
		if !envNameShape.MatchString(name) {
			return refuse("step variable %q is not a name the step's shell can read", name)
		}
	}
	taken := namesTheJobSets(run)
	for _, name := range sortedKeys(run.Secrets) {
		switch {
		case len(name) > 253 || !envNameShape.MatchString(name):
			return refuse("secret %q is not a variable name the step's shell can read", name)
		case strings.HasPrefix(name, platformPrefix):
			return refuse("secret %q is named in the platform's %s namespace", name, platformPrefix)
		case taken[name]:
			return refuse("secret %q collides with a variable the platform sets for the step", name)
		}
	}
	return int32(ttl), nil
}

// namesTheJobSets is every variable name the Job or the wrapper sets on its
// own account, in the step container or its Secret, that a step secret may
// therefore not take: the contract environment, the wrapper's variables, the
// step's git configuration, the declared caches' variables, and the clone
// token's key.
func namesTheJobSets(run StepRun) map[string]bool {
	taken := map[string]bool{
		stepCommandVar:    true,
		stepArtifactsVar:  true,
		artifactMarkerVar: true,
		stepCachesVar:     true,
		gitConfigCountVar: true,
		gitConfigKeyVar:   true,
		gitConfigValueVar: true,
		gitTokenKey:       true,
	}
	for name := range run.Env {
		taken[name] = true
	}
	for _, cache := range declaredCaches(run.Caches) {
		for _, name := range cacheVariables[cache] {
			taken[name] = true
		}
	}
	return taken
}

// cloneURLProblem says what is wrong with a clone URL, as the sentence a
// refusal carries, or "". Only plain https: a token sent over http is a token
// handed to the network, and every other scheme git speaks (file, ssh, ext)
// reaches something that is not the repository host. No user, query or
// fragment either: the URL is a plain value in the Job, so anything they hold
// would sit there in plain sight, and a clone URL has no use for them.
//
// The sentence becomes the step's failure text, in the run record and on the
// check run, so it never repeats the parts of a URL that can hold a
// credential, and never repeats a URL that does not parse -- nothing then says
// which part of it is the credential.
func cloneURLProblem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the clone URL does not parse as a URL"
	}
	if u.Opaque != "" || u.Host == "" {
		return "the clone URL names no host"
	}
	shown := strconv.Quote((&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String())
	switch {
	case u.User != nil:
		return "the clone URL " + shown + " carries a user, which would put a credential in the Job"
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "the clone URL " + shown + " carries a query or a fragment, which would put whatever they hold in the Job"
	case u.Scheme != "https":
		return "the clone URL " + shown + " is not an https URL"
	}
	return ""
}

// artifactProblem says what is wrong with an artifact path, or "". It is the
// wrapper's safety: the list reaches tar word-split, unquoted and GLOBBED, so a
// path must be one word that cannot name anything outside the working copy --
// not as written, and not as the shell expands it. On dash `.?` expands to
// `..` and `.*` to `. ..`, so besides a literal `.` or `..` segment, a glob
// segment that begins with a dot, or with a bracket that can stand for one, is
// refused. A plain dotfile (`.coverage`) and an ordinary glob (`dist/*.js`)
// stay legal: a leading dot must be matched explicitly, so a segment that
// begins with anything else cannot expand to one.
func artifactProblem(path string) string {
	switch {
	case path == "":
		return "is empty"
	case strings.HasPrefix(path, "/"):
		return "is absolute; artifact paths are relative to the working copy"
	}
	for _, r := range path {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r):
			return "contains whitespace or a control character"
		case strings.ContainsRune(artifactForbidden, r):
			return "contains the shell metacharacter " + strconv.QuoteRune(r)
		}
	}
	for _, segment := range strings.Split(path, "/") {
		switch {
		case segment == "." || segment == "..":
			return "names " + segment + " as a path segment, which could leave the working copy"
		case (strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "[")) && strings.ContainsAny(segment, "*?["):
			return "has the glob segment " + strconv.Quote(segment) + ": a glob segment may not begin with a dot or a bracket"
		}
	}
	return ""
}

// The capabilities a container gives up (review I2). Pod Security baseline
// refuses a pod that ADDS a capability, never the container runtime's default
// set, and that set holds NET_RAW: with it a root container on a bridge
// network (k3s's flannel) can forge ARP replies and read the traffic of the
// pods beside it, the mesh's included, and no NetworkPolicy governs that. So
// no container a pipeline runs keeps it.
const (
	capAll    = "ALL"
	capNetRaw = "NET_RAW"
)

// rootContext pins cache-prep and the clone to uid 0 (ruling R17): the cache
// claim's root is root-owned and may be 0755, so only root can open the
// owner's directory under it. Root by uid alone: both run a fixed script that
// needs none of the runtime's default capabilities -- an owner creates and
// chmods its own directories, and git fetches and checks out as anyone does --
// so they drop every one (measured on k3s v1.35, under a root-owned claim root
// of 0777 and of 0755). A claim whose root another uid owns and keeps closed
// fails cache-prep, naming the directory, rather than being forced open.
func rootContext() *SecurityContext {
	return &SecurityContext{
		RunAsUser:                ptr(int64(0)),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &Capabilities{Drop: []string{capAll}},
	}
}

// imageContext is the step's and every service's: the image's own user, no
// privilege escalation, and the runtime's default capabilities but NET_RAW.
// These are images the platform did not build, whose entrypoints use the rest
// of the set -- postgres's chowns its data directory and steps down to its own
// user -- so only the one that reaches the network beneath the pod goes.
func imageContext() *SecurityContext {
	return &SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &Capabilities{Drop: []string{capNetRaw}},
	}
}

// cachePrepContainer opens the step's directory of the cache claim before
// anything else runs (ruling R15b). It mounts the claim's ROOT and nothing
// else, and runs a fixed script: the container that sees every owner's cache
// is the one that runs no repository content.
func cachePrepContainer(cfg Config, run StepRun) Container {
	return Container{
		Name:                     ContainerCachePrep,
		Image:                    cfg.CloneImage,
		Command:                  []string{"/bin/sh", "-c", cachePrepScript},
		Env:                      []EnvVar{plainVar(cacheDirVar, cacheRootPath+"/"+stepCacheDir(run))},
		VolumeMounts:             []VolumeMount{{Name: cacheVolume, MountPath: cacheRootPath}},
		SecurityContext:          rootContext(),
		TerminationMessagePolicy: failureFromLogs,
	}
}

// cloneContainer fetches the commit into the workspace, as root. It runs git
// on untrusted repository content, so it never mounts the cache claim.
func cloneContainer(cfg Config, run StepRun, secretName string) Container {
	return Container{
		Name:    ContainerClone,
		Image:   cfg.CloneImage,
		Command: []string{"/bin/sh", "-c", cloneScript},
		Env: []EnvVar{
			plainVar("CLONE_URL", run.Repository.CloneURL),
			plainVar("SHA", run.SHA),
			// Optional: an anonymous clone of a public repository has no token,
			// and a required key that is absent would stop the pod.
			secretVar(gitTokenKey, secretName, gitTokenKey, true),
		},
		VolumeMounts:             []VolumeMount{{Name: workspaceVolume, MountPath: workspacePath}},
		SecurityContext:          rootContext(),
		TerminationMessagePolicy: failureFromLogs,
	}
}

// serviceContainers are the native sidecars, in name order: init containers
// with restartPolicy Always. They see neither the checkout nor the cache, get
// their declared plain values only, never the step's secrets, and may not gain
// privileges (an image's entrypoint dropping from root to its own user still
// works; a setuid binary does not) or keep NET_RAW (imageContext).
func serviceContainers(services map[string]pl.Service) []Container {
	var out []Container
	for _, name := range sortedKeys(services) {
		svc := services[name]
		c := Container{
			Name:                     ServicePrefix + name,
			Image:                    svc.Image,
			RestartPolicy:            ptr("Always"),
			SecurityContext:          imageContext(),
			TerminationMessagePolicy: failureFromLogs,
		}
		for _, key := range sortedKeys(svc.Env) {
			c.Env = append(c.Env, plainVar(key, svc.Env[key]))
		}
		if strings.TrimSpace(svc.Ready) != "" {
			c.StartupProbe = &Probe{
				// The kubelet expands a probe's command against the container's
				// values like an env value (measured), so the check is the
				// manifest's text through the same escape.
				Exec:             &ExecAction{Command: []string{"/bin/sh", "-c", kubeLiteral(svc.Ready)}},
				PeriodSeconds:    2,
				FailureThreshold: 90,
			}
		}
		out = append(out, c)
	}
	return out
}

func stepContainer(run StepRun, jobName, secretName string, caches []string) Container {
	env := make([]EnvVar, 0, len(run.Env)+len(run.Secrets)+8)
	for _, name := range sortedKeys(run.Env) {
		env = append(env, plainVar(name, run.Env[name]))
	}
	env = append(env, plainVar(stepCommandVar, run.Command))
	if len(run.Artifacts) > 0 {
		env = append(env, plainVar(stepArtifactsVar, strings.Join(run.Artifacts, " ")))
	}
	env = append(env,
		plainVar(artifactMarkerVar, ArtifactMarker(jobName)),
		plainVar(gitConfigCountVar, "1"),
		plainVar(gitConfigKeyVar, "safe.directory"),
		plainVar(gitConfigValueVar, workspacePath),
	)
	if len(caches) > 0 {
		env = append(env, plainVar(stepCachesVar, strings.Join(caches, " ")))
	}
	for _, name := range sortedKeys(run.Secrets) {
		env = append(env, secretVar(name, secretName, name, false))
	}

	mounts := []VolumeMount{{Name: workspaceVolume, MountPath: workspacePath}}
	if len(caches) > 0 {
		// The step's own directory only, never the claim's root (ruling R15b).
		mounts = append(mounts, VolumeMount{Name: cacheVolume, MountPath: cachePath, SubPath: stepCacheDir(run)})
	}
	return Container{
		Name:            ContainerStep,
		Image:           run.Image,
		Command:         []string{"/bin/sh", "-c", stepWrapper},
		WorkingDir:      workspacePath,
		Env:             env,
		VolumeMounts:    mounts,
		SecurityContext: imageContext(),
	}
}

// objectLabels are the labels on the Job, its pod template and its Secret.
func objectLabels(run StepRun) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedBy,
		LabelRun:       RunLabelValue(run.RunID),
		LabelAttempt:   strconv.Itoa(run.Attempt),
	}
}

// objectAnnotations carry what a label cannot hold (a step key has a slash
// and a #) and what nobody selects on.
func objectAnnotations(run StepRun) map[string]string {
	return map[string]string{
		AnnotStepKey: run.StepKey,
		AnnotWorkRun: run.WorkRunID,
		AnnotOwner:   run.OwnerUserID,
	}
}

// declaredCaches is the set of declared caches, sorted, so the same
// declaration always names the same caches in the same order.
func declaredCaches(caches []string) []string {
	seen := make(map[string]bool, len(caches))
	out := make([]string, 0, len(caches))
	for _, name := range caches {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// stepCacheDir is the step's directory of the cache claim: its owner's, its
// repository's, and the trust of the event its run was opened for. cache-prep
// opens exactly this directory and the step mounts exactly this one, so a
// run of the default branch never mounts a pull request's cache, nor the
// other way round.
func stepCacheDir(run StepRun) string {
	return CacheSubPath(run.OwnerUserID, run.Repository, cacheTrustOf(run))
}

// plainVar is a literal environment variable, written through kubeLiteral:
// every value BuildJob gives a container goes through here.
func plainVar(name, value string) EnvVar { return EnvVar{Name: name, Value: kubeLiteral(value)} }

// kubeLiteral writes s so that the container receives s itself (ruling R25).
// Kubernetes treats an env value, a command and an exec probe's command as a
// template: $(NAME) becomes the value of a variable defined before it and $$
// becomes $. So a step's `kill -9 $$` reached the shell as `kill -9 $`, and a
// quoted `$(MEMQL_RUN_ID)` arrived as the run id (measured on k3s v1.32). The
// step's command, the contract values, a service's values and its ready check
// are the manifest's text, or the event's, and must arrive as written, so
// every $ is doubled; the kubelet turns $$ back into $ and expands nothing.
// A secretKeyRef's value is not a template and is never passed through here.
func kubeLiteral(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func secretVar(name, secretName, key string, optional bool) EnvVar {
	ref := &SecretKeySelector{Name: secretName, Key: key}
	if optional {
		ref.Optional = ptr(true)
	}
	return EnvVar{Name: name, ValueFrom: &EnvVarSource{SecretKeyRef: ref}}
}

// jobOwner makes a Job the owner of what it is put on, so collecting the Job
// collects that too: not as its controller, and without holding the Job's
// deletion until the dependent is gone (which would need a grant on the Job's
// finalizers).
func jobOwner(job Job) OwnerReference {
	return OwnerReference{
		APIVersion:         "batch/v1",
		Kind:               "Job",
		Name:               job.Metadata.Name,
		UID:                job.Metadata.UID,
		Controller:         ptr(false),
		BlockOwnerDeletion: ptr(false),
	}
}

// ---------------------------------------------------------------------------
// The isolation probe (Task 6b, rulings R12, R42-R44)
// ---------------------------------------------------------------------------
//
// The proof that memql-pipelines is isolated (isolation.go) is ONE Indexed Job
// of two pods, so it takes one slot under the ceiling and waits for one as a
// step does (R43):
//
//	init  listener   cfg.CloneImage as a native sidecar, in both pods: accepts
//	                 and closes connections on port 8080. Its readiness probe
//	                 is the kubelet's own TCP connection, which no
//	                 NetworkPolicy governs (measured, k3s v1.35), so ready
//	                 means listening; it runs every second, and one miss is
//	                 not ready, so a listener that stopped accepting reads so
//	                 within a second. It depends on nothing, so it is up and
//	                 ready before the probe Secret exists.
//	main  connector  cfg.CloneImage. Its environment names two keys of the
//	                 probe Secret, not optionally, so the kubelet starts it
//	                 only once the Secret exists -- which the runner creates
//	                 only once index 0's listener is ready: the connector
//	                 cannot try before the listener listens. Index 1's is the
//	                 connector; index 0's only holds its pod, and so the
//	                 listener, up until the Job is deleted.
//
// The pods hold no credential and run as a step's do: the step identity, no
// token, no Service links, seccomp RuntimeDefault, no privilege escalation,
// and no run label, so no cancel of a run reaches them. Their containers keep
// no capability at all (probeContext).
//
// The connector's exit code is its verdict, which the runner reads together
// with its own re-read of the listener (ruling R42). After a settle of five
// seconds it makes three rounds, a second apart, each of a TCP connection to
// the cluster's DNS -- the reachable positive: the policy lets every pod reach
// it, so an answer proves the network works and the connector can connect --
// and one to index 0's listener, each bounded at five seconds (R44):
//
//	exit  the three rounds                                        verdict
//	20    no listener attempt connected (each was refused or      isolated, if the runner's
//	      timed out), and every DNS attempt connected             re-read finds the listener
//	                                                              still ready, the same
//	                                                              incarnation in the same
//	                                                              pod, Ready, which the API
//	                                                              server has not marked as
//	                                                              going away, and its kubelet
//	                                                              still answers through the
//	                                                              API server; inconclusive
//	                                                              otherwise
//	21    a listener attempt connected                            not isolated
//	22    a DNS attempt failed, and none to the listener          inconclusive
//	      connected
//	23    a listener attempt failed some other way ("No route     inconclusive
//	      to host"): neither refused nor timed out
//	24    the pod's resolv.conf names no nameserver               inconclusive
//	25    the probe Secret is not this Job's, or names nothing    inconclusive
//	26    the pod has no part for its completion index            inconclusive
//	any   other, 0 included: the connector ended with no verdict  inconclusive
//
// A refusal counts as unreachable (R42), not as a reset from the listener: a
// policy engine may reject rather than drop -- k3s's answers a denied SYN with
// an ICMP error the kernel reports as "Connection refused", exactly as it
// reports a reset (measured, k3s v1.35) -- and a listener that stayed ready
// had its socket open throughout, so a SYN that reached it would have been
// accepted, never reset.
//
// The settle is there because a policy engine programs a new pod's rules a
// moment after the pod starts: k3s's within two seconds (measured). A policy
// that programs slower than the settle can only push the verdict toward "not
// isolated" or "inconclusive" -- the safe directions -- never toward a false
// pass. A rule not yet in place lets more through, never less: it can turn an
// attempt that would have failed into one that connects -- a listener
// attempt that connects is "not isolated" -- but it cannot make an attempt
// fail. A pass rests on every listener attempt failing, which only rules in
// place bring about; and a network not yet working for the new pod fails its
// DNS attempts too, which is "inconclusive". (A DNS-blocking rule not yet in
// place lets a DNS attempt through that it would refuse later: that takes away
// a reason for "inconclusive", but never makes a pass, which still needs every
// listener attempt stopped by a rule already in place.)

const (
	// probePort is the port every probe pod's listener accepts on.
	probePort int32 = 8080
	// The connector's exit codes: the table above.
	probeExitIsolated      = 20
	probeExitConnected     = 21
	probeExitDNS           = 22
	probeExitUnclear       = 23
	probeExitNoNameserver  = 24
	probeExitForeignTarget = 25
	probeExitNoPart        = 26
	// The probe Secret's keys, and the connector's variables: the two that
	// read them, and its own Job's uid from its pod's label.
	probeTargetKey  = "target"
	probeJobUIDKey  = "job-uid"
	probeTargetVar  = "MEMQL_PROBE_TARGET"
	probeJobUIDVar  = "MEMQL_PROBE_JOB_UID"
	probeSelfUIDVar = "MEMQL_PROBE_SELF_UID"
	// probeGracePeriodSeconds: the probe's pods hold nothing worth a wait.
	probeGracePeriodSeconds = 2
	// probeJobTTL collects a probe Job a runner went before deleting: it
	// holds a slot under the ceiling until it is gone, and nothing ever reads
	// it.
	probeJobTTL = 60 * time.Second
	// probeJobDeadline is the probe Job's whole life, past the runner's
	// bounds on its pods coming up and its connector ending, so a probe whose
	// runner went ends on its own.
	probeJobDeadline = probeUpTimeout + probeEndTimeout + 30*time.Second
)

// probeListenerScript is each probe pod's listener: perl, which git depends
// on, so the clone image has it. It accepts and closes connections on 8080
// until it is stopped, handling TERM itself because, as the container's first
// process, it gets no signal it has not asked for. A listener that cannot take
// the port ends at once, saying so, and is never ready.
const probeListenerScript = `exec perl -MIO::Socket::IP -e '$SIG{TERM} = sub { exit 0 }; my $s = IO::Socket::IP->new(LocalPort => 8080, Listen => 64, ReuseAddr => 1) or die "memql: the isolation probe cannot listen on 8080: $@\n"; while (1) { my $c = $s->accept or next; close $c }'`

// probeConnectorScript is each probe pod's main container, under bash, whose
// /dev/tcp makes the connections. Index 0's holds its pod up; index 1's is the
// connector, whose exit code is the table above. The five assignments at its
// head are its tunables, each on a line of its own; the tests point them at
// local endpoints (TestIsolationProbeConnectorReadsTheThreeRoundsTogether). An
// attempt is classified by timeout's 124 and by bash's own sentence for a
// refusal; whatever else ends one is neither. The line it prints last is the
// one the runner quotes, from the container's termination message.
const probeConnectorScript = `case "${JOB_COMPLETION_INDEX:-}" in
1) ;;
0)
  trap 'exit 0' TERM
  while :; do sleep 1; done ;;
*)
  echo "memql: isolation probe: no part for completion index ${JOB_COMPLETION_INDEX:-(none)}"
  exit 26 ;;
esac
resolv=/etc/resolv.conf
dns_port=53
listener_port=8080
settle=5
limit=5
if [ -z "${MEMQL_PROBE_SELF_UID:-}" ] || [ "${MEMQL_PROBE_JOB_UID:-}" != "$MEMQL_PROBE_SELF_UID" ] || [ -z "${MEMQL_PROBE_TARGET:-}" ]; then
  echo "memql: isolation probe: the target Secret is not this probe Job's (it names Job ${MEMQL_PROBE_JOB_UID:-none}, this pod's is ${MEMQL_PROBE_SELF_UID:-unknown}, listener ${MEMQL_PROBE_TARGET:-none})"
  exit 25
fi
ns=$(awk '$1 == "nameserver" { print $2; exit }' "$resolv")
if [ -z "$ns" ]; then
  echo "memql: isolation probe: $resolv names no nameserver"
  exit 24
fi
attempt() {
  out=$(timeout "$limit" bash -c 'exec 3<>"/dev/tcp/$1/$2"' attempt "$1" "$2" 2>&1)
  case $? in
  0) echo connected ;;
  124) echo "timed out" ;;
  *)
    case "$out" in
    *"Connection refused"*) echo refused ;;
    *) echo "failed (${out##*: })" ;;
    esac ;;
  esac
}
sleep "$settle"
dns="" listener="" connected=0 dns_failed=0 unclear=0
for round in 1 2 3; do
  if [ "$round" != 1 ]; then sleep 1; fi
  d=$(attempt "$ns" "$dns_port")
  l=$(attempt "$MEMQL_PROBE_TARGET" "$listener_port")
  if [ "$d" != connected ]; then dns_failed=1; fi
  case "$l" in
  connected) connected=1 ;;
  refused | "timed out") ;;
  *) unclear=1 ;;
  esac
  dns="$dns${dns:+, }$d"
  listener="$listener${listener:+, }$l"
done
echo "memql: isolation probe: dns $ns:$dns_port $dns; listener $MEMQL_PROBE_TARGET:$listener_port $listener"
if [ "$connected" = 1 ]; then exit 21; fi
if [ "$dns_failed" = 1 ]; then exit 22; fi
if [ "$unclear" = 1 ]; then exit 23; fi
exit 20`

// probeResources is each probe container's: the listener and the connector
// use a few MiB each, and the LimitRange's defaults (250m and 512Mi) would ask
// a whole CPU and 2 GiB for the probe's four containers -- more than a step,
// on a node a step must fit beside.
func probeResources() *Resources {
	return &Resources{
		Requests: map[string]string{"cpu": "10m", "memory": "32Mi"},
		Limits:   map[string]string{"cpu": "100m", "memory": "64Mi"},
	}
}

// probeContext is each probe container's: the clone image's own user, no
// privilege escalation, and every capability dropped. A listener on an
// unprivileged port and a TCP connection need none of them -- which matters
// in one direction above all: a connector that could not connect would read
// as isolated. Measured on k3s v1.35 with every capability dropped: under the
// policy the proof passes, and with the policy deleted the connector reaches
// the listener and the proof refuses.
func probeContext() *SecurityContext {
	return &SecurityContext{AllowPrivilegeEscalation: ptr(false), Capabilities: &Capabilities{Drop: []string{capAll}}}
}

// probeLabels are the labels on the probe Job, its pods and its Secret.
func probeLabels() map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelProbe: ProbeIsolation}
}

// BuildIsolationProbe is the probe's Indexed Job (above), named by
// IsolationProbeName: a pure function of its inputs.
func BuildIsolationProbe(cfg Config, name string) Job {
	target := IsolationTargetName(name)
	return Job{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   ObjectMeta{Name: name, Namespace: cfg.Namespace, Labels: probeLabels()},
		Spec: JobSpec{
			// Two pods, one per index, at once. A per-index limit of zero
			// retries nothing, and the connector failing -- every verdict is
			// a non-zero exit -- ends neither the listener nor the Job; a
			// Job-wide backoffLimit of 0 would end both. Left unset, it
			// defaults to unlimited when a per-index limit is set.
			CompletionMode:          "Indexed",
			Completions:             ptr(int32(2)),
			Parallelism:             ptr(int32(2)),
			BackoffLimitPerIndex:    ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(probeJobDeadline / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(probeJobTTL / time.Second)),
			Template: PodTemplateSpec{
				Metadata: ObjectMeta{Labels: probeLabels()},
				Spec: PodSpec{
					RestartPolicy:                 "Never",
					ServiceAccountName:            cfg.StepServiceAccount,
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					TerminationGracePeriodSeconds: ptr(int64(probeGracePeriodSeconds)),
					SecurityContext:               &PodSecurityContext{SeccompProfile: &SeccompProfile{Type: "RuntimeDefault"}},
					InitContainers: []Container{{
						Name:                     ContainerProbeListener,
						Image:                    cfg.CloneImage,
						Command:                  []string{"/bin/sh", "-c", probeListenerScript},
						RestartPolicy:            ptr("Always"),
						ReadinessProbe:           &Probe{TCPSocket: &TCPSocketAction{Port: probePort}, PeriodSeconds: 1, FailureThreshold: 1},
						Resources:                probeResources(),
						SecurityContext:          probeContext(),
						TerminationMessagePolicy: failureFromLogs,
					}},
					Containers: []Container{{
						Name:    ContainerProbeConnector,
						Image:   cfg.CloneImage,
						Command: []string{"/bin/bash", "-c", probeConnectorScript},
						Env: []EnvVar{
							secretVar(probeTargetVar, target, probeTargetKey, false),
							secretVar(probeJobUIDVar, target, probeJobUIDKey, false),
							{Name: probeSelfUIDVar, ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{
								FieldPath: "metadata.labels['" + controllerUIDLabel + "']",
							}}},
						},
						Resources:                probeResources(),
						SecurityContext:          probeContext(),
						TerminationMessagePolicy: failureFromLogs,
					}},
				},
			},
		},
	}
}

// BuildIsolationTarget is the probe Secret: index 0's address, which the
// connector dials, and the probe Job's uid, which it checks against its own
// pod's -- a Secret an earlier probe left would name another Job, and an
// address that may be anybody's now. It is owned by the probe Job from the
// moment it exists (R43), so it goes with the Job and the orphan-Secret sweep,
// which judges the ownerless, never sees it. Refused without the Job's uid,
// which only the API server's answer carries, or without the address.
func BuildIsolationTarget(cfg Config, probe Job, listenerIP string) (Secret, error) {
	listenerIP = strings.TrimSpace(listenerIP)
	switch {
	case probe.Metadata.Name == "" || probe.Metadata.UID == "":
		return Secret{}, fmt.Errorf("pipelinesteps: probe Job %q has no uid to own its Secret by (use the Job the API server returned)", probe.Metadata.Name)
	case listenerIP == "":
		return Secret{}, fmt.Errorf("pipelinesteps: the probe Secret of %s names no listener address", probe.Metadata.Name)
	}
	return Secret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: ObjectMeta{
			Name:            IsolationTargetName(probe.Metadata.Name),
			Namespace:       cfg.Namespace,
			Labels:          probeLabels(),
			OwnerReferences: []OwnerReference{jobOwner(probe)},
		},
		Type: "Opaque",
		Data: map[string][]byte{probeTargetKey: []byte(listenerIP), probeJobUIDKey: []byte(probe.Metadata.UID)},
	}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ptr[T any](v T) *T { return &v }
