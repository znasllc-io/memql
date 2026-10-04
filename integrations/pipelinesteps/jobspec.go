package pipelinesteps

import (
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
//	                     at /cache-root, opens the owner's directory -- no git,
//	                     no token, no checkout
//	init  clone          cfg.CloneImage as root: shallow-fetches the SHA into
//	                     /workspace, the ONLY container that can read the clone
//	                     token; never sees the cache claim
//	init  svc-<name>...  one native sidecar per service, sorted by name; the
//	                     kubelet starts the next container only once a
//	                     sidecar's startup probe passes
//	main  step           the manifest's image as its own user, running the
//	                     command through stepWrapper in /workspace, with the
//	                     contract environment, the step's secrets by reference,
//	                     and only the owner's directory of the cache at /cache
//
// No cluster credential of any kind is present: the pod runs as an identity
// with no RoleBinding, its token is never mounted, Service links are off, and
// the only Secret it can name is its own. No container may gain privileges.
//
// BuildJob and BuildSecret are pure: the same inputs give the same objects,
// which is what lets a replica rebuild what another replica created.

const (
	workspaceVolume = "workspace"
	workspacePath   = "/workspace"
	cacheVolume     = "cache"
	cachePath       = "/cache"

	// cacheRootPath is where cache-prep mounts the cache claim's ROOT, and
	// ownerCacheVar names the owner's directory under it that cache-prep opens,
	// world-writable and sticky (rulings R15, R15b). Kubelet creates a missing
	// subPath root-owned with the claim root's mode, so on a claim whose root is
	// not world-writable a non-root step would get a cache it cannot enter; the
	// sticky bit keeps one uid of the owner's steps from renaming or replacing
	// another uid's tree. Only cache-prep -- a fixed script, no git, no token --
	// ever sees the claim's root, where every owner's cache lives.
	cacheRootPath = "/cache-root"
	ownerCacheVar = "OWNER_CACHE"

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

// rootContext pins cache-prep and the clone to uid 0 (ruling R17): the cache
// claim's root is root-owned and may be 0755, so only root can open the
// owner's directory under it.
func rootContext() *SecurityContext {
	return &SecurityContext{RunAsUser: ptr(int64(0)), AllowPrivilegeEscalation: ptr(false)}
}

// cachePrepContainer opens the owner's directory of the cache claim before
// anything else runs (ruling R15b). It mounts the claim's ROOT and nothing
// else, and runs a fixed script: the container that sees every owner's cache
// is the one that runs no repository content.
func cachePrepContainer(cfg Config, run StepRun) Container {
	return Container{
		Name:                     ContainerCachePrep,
		Image:                    cfg.CloneImage,
		Command:                  []string{"/bin/sh", "-c", cachePrepScript},
		Env:                      []EnvVar{plainVar(ownerCacheVar, cacheRootPath+"/"+CacheSubPath(run.OwnerUserID))},
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
// works; a setuid binary does not).
func serviceContainers(services map[string]pl.Service) []Container {
	var out []Container
	for _, name := range sortedKeys(services) {
		svc := services[name]
		c := Container{
			Name:                     ServicePrefix + name,
			Image:                    svc.Image,
			RestartPolicy:            ptr("Always"),
			SecurityContext:          &SecurityContext{AllowPrivilegeEscalation: ptr(false)},
			TerminationMessagePolicy: failureFromLogs,
		}
		for _, key := range sortedKeys(svc.Env) {
			c.Env = append(c.Env, plainVar(key, svc.Env[key]))
		}
		if strings.TrimSpace(svc.Ready) != "" {
			c.StartupProbe = &Probe{
				Exec:             &ExecAction{Command: []string{"/bin/sh", "-c", svc.Ready}},
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
		// The owner's directory only, never the claim's root (ruling R15).
		mounts = append(mounts, VolumeMount{Name: cacheVolume, MountPath: cachePath, SubPath: CacheSubPath(run.OwnerUserID)})
	}
	return Container{
		Name:            ContainerStep,
		Image:           run.Image,
		Command:         []string{"/bin/sh", "-c", stepWrapper},
		WorkingDir:      workspacePath,
		Env:             env,
		VolumeMounts:    mounts,
		SecurityContext: &SecurityContext{AllowPrivilegeEscalation: ptr(false)},
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

func plainVar(name, value string) EnvVar { return EnvVar{Name: name, Value: value} }

func secretVar(name, secretName, key string, optional bool) EnvVar {
	ref := &SecretKeySelector{Name: secretName, Key: key}
	if optional {
		ref.Optional = ptr(true)
	}
	return EnvVar{Name: name, ValueFrom: &EnvVarSource{SecretKeyRef: ref}}
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
