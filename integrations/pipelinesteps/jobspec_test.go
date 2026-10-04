package pipelinesteps

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// testJobName is JobName("run-7f3a", "tests/go-tests#2", 2), pinned in
// names_test.go.
const testJobName = "mp-a7a72726d5075767e0b6d115"

// testSecretName is the Secret every reference in testJobName's Job names.
const testSecretName = testJobName + "-env"

// Planted values: distinctive enough that finding one in a marshalled Job can
// only mean it leaked, and assembled from parts so a secret scanner reads
// them as the fixtures they are.
var (
	plantedNPM    = "npm-" + strings.Repeat("z", 12)
	plantedDeploy = "deploy-" + strings.Repeat("y", 12)
	plantedClone  = "clone-" + strings.Repeat("x", 12)
)

var testSHA = strings.Repeat("ab12", 10)

// testConfig deliberately differs from every default, so a Job that carries
// one of these values took it from the Config rather than from a literal.
func testConfig() Config {
	return Config{
		Namespace:          "steps-ns",
		CloneImage:         "registry.example.com/library/git:2",
		CacheClaim:         "steps-cache",
		StepServiceAccount: "steps-sa",
		JobTTL:             25 * time.Minute,
	}
}

func testRun() StepRun {
	return StepRun{
		RunID:       "run-7f3a",
		WorkRunID:   "work-91c2",
		StepKey:     "tests/go-tests#2",
		Attempt:     2,
		OwnerUserID: "user-5d1e",
		Repository: pl.Repository{
			Owner:    "acme",
			Name:     "widget",
			CloneURL: "https://github.com/acme/widget.git",
		},
		SHA:            testSHA,
		InstallationID: 42,
		Image:          "registry.example.com/acme/toolchain:1.4",
		Command:        "go test $MEMQL_PACKAGES",
		Env: map[string]string{
			"MEMQL_STEP":     "tests/go-tests#2",
			"MEMQL_RUN_ID":   "run-7f3a",
			"MEMQL_SHA":      testSHA,
			"MEMQL_PACKAGES": "./a/... ./b",
		},
		Secrets: map[string]string{
			"NPM_TOKEN":  plantedNPM,
			"DEPLOY_KEY": plantedDeploy,
		},
		Services: map[string]pl.Service{
			"redis": {Image: "redis:7"},
			"postgres": {
				Image: "postgres:16",
				Env:   map[string]string{"POSTGRES_USER": "memql", "POSTGRES_PASSWORD": "memql"},
				Ready: "pg_isready -U memql",
			},
		},
		Caches:         []string{"npm", "go"},
		Artifacts:      []string{"coverage.out", "reports/*.xml"},
		TimeoutSeconds: 900,
		DeadlineCode:   pl.CodeStepTimeout,
	}
}

func mustBuild(t *testing.T, cfg Config, run StepRun) Job {
	t.Helper()
	job, err := BuildJob(cfg, run, testJobName)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	return job
}

// wire marshals v and decodes it generically, so an assertion reads the field
// names the API server receives rather than the Go names.
func wire(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

// field walks a decoded document by map key (string) and list index (int).
func field(doc any, path ...any) (any, bool) {
	cur := doc
	for _, step := range path {
		switch key := step.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[key]; !ok {
				return nil, false
			}
		case int:
			list, ok := cur.([]any)
			if !ok || key < 0 || key >= len(list) {
				return nil, false
			}
			cur = list[key]
		default:
			return nil, false
		}
	}
	return cur, true
}

func wantField(t *testing.T, doc any, want any, path ...any) {
	t.Helper()
	got, ok := field(doc, path...)
	if !ok {
		t.Errorf("%v is absent from the wire, want %#v", path, want)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v = %#v on the wire, want %#v", path, got, want)
	}
}

func envNamed(c Container, name string) (EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return EnvVar{}, false
}

func allContainers(job Job) []Container {
	pod := job.Spec.Template.Spec
	return append(append([]Container{}, pod.InitContainers...), pod.Containers...)
}

// stepOf and cloneOf fail the test, rather than panic the binary, on a Job
// that lacks the container.
func stepOf(t *testing.T, job Job) Container {
	t.Helper()
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("%d main containers, want exactly the step", len(containers))
	}
	return containers[0]
}

func cloneOf(t *testing.T, job Job) Container {
	t.Helper()
	init := job.Spec.Template.Spec.InitContainers
	if len(init) == 0 {
		t.Fatal("no init containers: nothing clones the repository")
	}
	return init[0]
}

func ptrTo[T any](v T) *T { return &v }

func TestBuildJob(t *testing.T) {
	t.Run("the Job never retries, dies at the step's deadline and is collected after the TTL", func(t *testing.T) {
		job := mustBuild(t, testConfig(), testRun())
		spec := job.Spec
		if spec.BackoffLimit == nil || *spec.BackoffLimit != 0 {
			t.Errorf("backoffLimit = %v, want an explicit 0: absent means six retries of a failed step", spec.BackoffLimit)
		}
		if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 900 {
			t.Errorf("activeDeadlineSeconds = %v, want 900 (the run's effective timeout)", spec.ActiveDeadlineSeconds)
		}
		if spec.TTLSecondsAfterFinished == nil || *spec.TTLSecondsAfterFinished != 1500 {
			t.Errorf("ttlSecondsAfterFinished = %v, want 1500 (the Config's 25 minutes)", spec.TTLSecondsAfterFinished)
		}

		doc := wire(t, job)
		wantField(t, doc, "batch/v1", "apiVersion")
		wantField(t, doc, "Job", "kind")
		wantField(t, doc, float64(0), "spec", "backoffLimit")
		wantField(t, doc, float64(900), "spec", "activeDeadlineSeconds")
		wantField(t, doc, float64(1500), "spec", "ttlSecondsAfterFinished")
	})

	t.Run("a step with no time left is refused rather than given an unbounded Job", func(t *testing.T) {
		for _, secs := range []int{0, -30} {
			run := testRun()
			run.TimeoutSeconds = secs
			job, err := BuildJob(testConfig(), run, testJobName)
			if err == nil {
				t.Errorf("TimeoutSeconds %d: BuildJob succeeded; a Job with no deadline would run until the TTL", secs)
			}
			if !reflect.DeepEqual(job, Job{}) {
				t.Errorf("TimeoutSeconds %d: a refusal returned a half-built Job: %+v", secs, job)
			}
		}
	})

	t.Run("the pod never restarts and holds no credential of the cluster's", func(t *testing.T) {
		job := mustBuild(t, testConfig(), testRun())
		pod := job.Spec.Template.Spec
		if pod.RestartPolicy != "Never" {
			t.Errorf("restartPolicy = %q, want Never: a Job pod that restarts re-runs the step", pod.RestartPolicy)
		}
		if pod.ServiceAccountName != "steps-sa" {
			t.Errorf("serviceAccountName = %q, want the Config's step identity steps-sa", pod.ServiceAccountName)
		}
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Errorf("automountServiceAccountToken = %v, want an explicit false: absent mounts a token", pod.AutomountServiceAccountToken)
		}
		if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
			t.Errorf("enableServiceLinks = %v, want an explicit false: absent injects every Service's address", pod.EnableServiceLinks)
		}
		if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil ||
			pod.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
			t.Errorf("pod securityContext = %+v, want seccompProfile.type RuntimeDefault", pod.SecurityContext)
		}

		doc := wire(t, job)
		wantField(t, doc, "Never", "spec", "template", "spec", "restartPolicy")
		wantField(t, doc, "steps-sa", "spec", "template", "spec", "serviceAccountName")
		wantField(t, doc, false, "spec", "template", "spec", "automountServiceAccountToken")
		wantField(t, doc, false, "spec", "template", "spec", "enableServiceLinks")
		wantField(t, doc, "RuntimeDefault", "spec", "template", "spec", "securityContext", "seccompProfile", "type")
	})

	t.Run("the clone init container fetches the SHA, the only one offered the token, optionally", func(t *testing.T) {
		job := mustBuild(t, testConfig(), testRun())
		pod := job.Spec.Template.Spec
		if len(pod.InitContainers) == 0 {
			t.Fatal("no init containers: nothing clones the repository")
		}
		clone := pod.InitContainers[0]
		if clone.Name != "clone" {
			t.Errorf("first init container = %q, want clone (it must finish before any service starts)", clone.Name)
		}
		if clone.Image != "registry.example.com/library/git:2" {
			t.Errorf("clone image = %q, want the Config's clone image", clone.Image)
		}
		if !reflect.DeepEqual(clone.Command, []string{"/bin/sh", "-c", cloneScript}) {
			t.Errorf("clone command = %q, want /bin/sh -c <cloneScript>", clone.Command)
		}
		wantEnv := []EnvVar{
			{Name: "CLONE_URL", Value: "https://github.com/acme/widget.git"},
			{Name: "SHA", Value: testSHA},
			{Name: "GIT_TOKEN", ValueFrom: &EnvVarSource{SecretKeyRef: &SecretKeySelector{
				Name: testSecretName, Key: "GIT_TOKEN", Optional: ptrTo(true),
			}}},
		}
		if !reflect.DeepEqual(clone.Env, wantEnv) {
			t.Errorf("clone env =\n  %s\nwant\n  %s", mustJSON(t, clone.Env), mustJSON(t, wantEnv))
		}
		if !reflect.DeepEqual(clone.VolumeMounts, []VolumeMount{{Name: "workspace", MountPath: "/workspace"}}) {
			t.Errorf("clone mounts = %+v, want only the workspace at /workspace", clone.VolumeMounts)
		}
		if clone.SecurityContext == nil || clone.SecurityContext.AllowPrivilegeEscalation == nil ||
			*clone.SecurityContext.AllowPrivilegeEscalation {
			t.Errorf("clone securityContext = %+v, want allowPrivilegeEscalation false", clone.SecurityContext)
		}
		if clone.RestartPolicy != nil {
			t.Errorf("clone restartPolicy = %q: an init container with one is a sidecar, which never finishes, so the step would never start", *clone.RestartPolicy)
		}

		doc := wire(t, job)
		wantField(t, doc, "GIT_TOKEN", "spec", "template", "spec", "initContainers", 0, "env", 2, "valueFrom", "secretKeyRef", "key")
		wantField(t, doc, true, "spec", "template", "spec", "initContainers", 0, "env", 2, "valueFrom", "secretKeyRef", "optional")
		wantField(t, doc, false, "spec", "template", "spec", "initContainers", 0, "securityContext", "allowPrivilegeEscalation")
		if _, ok := field(doc, "spec", "template", "spec", "initContainers", 0, "restartPolicy"); ok {
			t.Error("the clone container carries a restartPolicy on the wire")
		}
	})

	t.Run("each service is a native sidecar, in name order, probed when it declares ready", func(t *testing.T) {
		job := mustBuild(t, testConfig(), testRun())
		init := job.Spec.Template.Spec.InitContainers
		var names []string
		for _, c := range init {
			names = append(names, c.Name)
		}
		if !reflect.DeepEqual(names, []string{"clone", "svc-postgres", "svc-redis"}) {
			t.Fatalf("init containers = %q, want clone, svc-postgres, svc-redis (services sorted by name)", names)
		}

		pg, redis := init[1], init[2]
		if pg.Image != "postgres:16" || redis.Image != "redis:7" {
			t.Errorf("service images = %q, %q, want postgres:16, redis:7", pg.Image, redis.Image)
		}
		for _, svc := range []Container{pg, redis} {
			if svc.RestartPolicy == nil || *svc.RestartPolicy != "Always" {
				t.Errorf("%s restartPolicy = %v, want Always (what makes an init container a native sidecar)", svc.Name, svc.RestartPolicy)
			}
			if len(svc.VolumeMounts) != 0 {
				t.Errorf("%s mounts %+v: a service sees neither the checkout nor the cache", svc.Name, svc.VolumeMounts)
			}
			for _, e := range svc.Env {
				if e.ValueFrom != nil {
					t.Errorf("%s env %s is a reference: a service gets its declared plain values, never the step's secrets", svc.Name, e.Name)
				}
			}
		}
		wantPGEnv := []EnvVar{{Name: "POSTGRES_PASSWORD", Value: "memql"}, {Name: "POSTGRES_USER", Value: "memql"}}
		if !reflect.DeepEqual(pg.Env, wantPGEnv) {
			t.Errorf("svc-postgres env = %+v, want %+v (sorted by name)", pg.Env, wantPGEnv)
		}
		wantProbe := &Probe{
			Exec:             &ExecAction{Command: []string{"/bin/sh", "-c", "pg_isready -U memql"}},
			PeriodSeconds:    2,
			FailureThreshold: 90,
		}
		if !reflect.DeepEqual(pg.StartupProbe, wantProbe) {
			t.Errorf("svc-postgres startupProbe = %s, want %s", mustJSON(t, pg.StartupProbe), mustJSON(t, wantProbe))
		}
		if redis.StartupProbe != nil {
			t.Errorf("svc-redis startupProbe = %+v, want none: it declared no ready probe", redis.StartupProbe)
		}

		doc := wire(t, job)
		wantField(t, doc, "Always", "spec", "template", "spec", "initContainers", 1, "restartPolicy")
		wantField(t, doc, []any{"/bin/sh", "-c", "pg_isready -U memql"},
			"spec", "template", "spec", "initContainers", 1, "startupProbe", "exec", "command")
		wantField(t, doc, float64(2), "spec", "template", "spec", "initContainers", 1, "startupProbe", "periodSeconds")
		wantField(t, doc, float64(90), "spec", "template", "spec", "initContainers", 1, "startupProbe", "failureThreshold")
		wantField(t, doc, "Always", "spec", "template", "spec", "initContainers", 2, "restartPolicy")
		if _, ok := field(doc, "spec", "template", "spec", "initContainers", 2, "startupProbe"); ok {
			t.Error("svc-redis carries a startupProbe on the wire")
		}
	})

	t.Run("the step runs the manifest image through the wrapper with the contract environment", func(t *testing.T) {
		job := mustBuild(t, testConfig(), testRun())
		containers := job.Spec.Template.Spec.Containers
		if len(containers) != 1 {
			t.Fatalf("%d main containers, want exactly the step", len(containers))
		}
		step := containers[0]
		if step.Name != "step" {
			t.Errorf("main container = %q, want step", step.Name)
		}
		if step.Image != "registry.example.com/acme/toolchain:1.4" {
			t.Errorf("step image = %q, want the manifest's image", step.Image)
		}
		if !reflect.DeepEqual(step.Command, []string{"/bin/sh", "-c", stepWrapper}) {
			t.Errorf("step command = %q, want /bin/sh -c <stepWrapper>", step.Command)
		}
		if step.WorkingDir != "/workspace" {
			t.Errorf("step workingDir = %q, want /workspace", step.WorkingDir)
		}

		ref := func(key string) *EnvVarSource {
			return &EnvVarSource{SecretKeyRef: &SecretKeySelector{Name: testSecretName, Key: key}}
		}
		wantEnv := []EnvVar{
			{Name: "MEMQL_PACKAGES", Value: "./a/... ./b"},
			{Name: "MEMQL_RUN_ID", Value: "run-7f3a"},
			{Name: "MEMQL_SHA", Value: testSHA},
			{Name: "MEMQL_STEP", Value: "tests/go-tests#2"},
			{Name: "MEMQL_STEP_COMMAND", Value: "go test $MEMQL_PACKAGES"},
			{Name: "MEMQL_STEP_ARTIFACTS", Value: "coverage.out reports/*.xml"},
			{Name: "MEMQL_ARTIFACT_MARKER", Value: "::memql-artifacts::4887b49fa27936d6"},
			{Name: "GOMODCACHE", Value: "/cache/go/mod"},
			{Name: "GOCACHE", Value: "/cache/go/build"},
			{Name: "npm_config_cache", Value: "/cache/npm"},
			{Name: "DEPLOY_KEY", ValueFrom: ref("DEPLOY_KEY")},
			{Name: "NPM_TOKEN", ValueFrom: ref("NPM_TOKEN")},
		}
		if !reflect.DeepEqual(step.Env, wantEnv) {
			t.Errorf("step env =\n  %s\nwant\n  %s", mustJSON(t, step.Env), mustJSON(t, wantEnv))
		}
		wantMounts := []VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "cache", MountPath: "/cache"}}
		if !reflect.DeepEqual(step.VolumeMounts, wantMounts) {
			t.Errorf("step mounts = %+v, want %+v", step.VolumeMounts, wantMounts)
		}
		if step.SecurityContext == nil || step.SecurityContext.AllowPrivilegeEscalation == nil ||
			*step.SecurityContext.AllowPrivilegeEscalation {
			t.Errorf("step securityContext = %+v, want allowPrivilegeEscalation false", step.SecurityContext)
		}
		if step.RestartPolicy != nil {
			t.Errorf("step restartPolicy = %q, want none on a main container", *step.RestartPolicy)
		}

		doc := wire(t, job)
		wantField(t, doc, false, "spec", "template", "spec", "containers", 0, "securityContext", "allowPrivilegeEscalation")
		wantField(t, doc, "/workspace", "spec", "template", "spec", "containers", 0, "workingDir")
	})

	t.Run("a step declaring no artifacts carries no artifact list", func(t *testing.T) {
		run := testRun()
		run.Artifacts = nil
		step := stepOf(t, mustBuild(t, testConfig(), run))
		if e, ok := envNamed(step, "MEMQL_STEP_ARTIFACTS"); ok {
			t.Errorf("MEMQL_STEP_ARTIFACTS = %q with nothing declared: the wrapper frames an archive whenever it is set", e.Value)
		}
	})

	t.Run("caches point their tools at the cache volume, and an unknown cache is refused", func(t *testing.T) {
		cacheEnv := func(c Container) []EnvVar {
			var out []EnvVar
			for _, e := range c.Env {
				switch e.Name {
				case "GOMODCACHE", "GOCACHE", "npm_config_cache":
					out = append(out, e)
				}
			}
			return out
		}
		goEnv := []EnvVar{{Name: "GOMODCACHE", Value: "/cache/go/mod"}, {Name: "GOCACHE", Value: "/cache/go/build"}}
		npmEnv := []EnvVar{{Name: "npm_config_cache", Value: "/cache/npm"}}
		for _, tc := range []struct {
			caches []string
			want   []EnvVar
		}{
			{[]string{"go"}, goEnv},
			{[]string{"npm"}, npmEnv},
			{[]string{"npm", "go", "go"}, append(append([]EnvVar{}, goEnv...), npmEnv...)},
		} {
			run := testRun()
			run.Caches = tc.caches
			step := stepOf(t, mustBuild(t, testConfig(), run))
			if got := cacheEnv(step); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("caches %q: cache env = %+v, want %+v", tc.caches, got, tc.want)
			}
		}

		run := testRun()
		run.Caches = nil
		job := mustBuild(t, testConfig(), run)
		step := stepOf(t, job)
		if got := cacheEnv(step); len(got) != 0 {
			t.Errorf("no caches declared, cache env = %+v", got)
		}
		for _, m := range step.VolumeMounts {
			if m.Name == "cache" || m.MountPath == "/cache" {
				t.Errorf("no caches declared, the step still mounts %+v", m)
			}
		}

		run.Caches = []string{"go", "maven"}
		if job, err := BuildJob(testConfig(), run, testJobName); err == nil || !reflect.DeepEqual(job, Job{}) {
			t.Errorf("an unknown cache built a Job (err %v); the runner has nowhere to point it", err)
		}
	})

	t.Run("the workspace is scratch and the cache claim is mounted only when caches are declared", func(t *testing.T) {
		job := mustBuild(t, testConfig(), testRun())
		wantVolumes := []Volume{
			{Name: "workspace", EmptyDir: &EmptyDirVolumeSource{}},
			{Name: "cache", PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "steps-cache"}},
		}
		if got := job.Spec.Template.Spec.Volumes; !reflect.DeepEqual(got, wantVolumes) {
			t.Errorf("volumes = %s, want %s", mustJSON(t, got), mustJSON(t, wantVolumes))
		}
		doc := wire(t, job)
		// Present as an object: a volume with no source is refused by the API server.
		wantField(t, doc, map[string]any{}, "spec", "template", "spec", "volumes", 0, "emptyDir")
		wantField(t, doc, "steps-cache", "spec", "template", "spec", "volumes", 1, "persistentVolumeClaim", "claimName")

		run := testRun()
		run.Caches = nil
		bare := mustBuild(t, testConfig(), run)
		if got := bare.Spec.Template.Spec.Volumes; !reflect.DeepEqual(got, wantVolumes[:1]) {
			t.Errorf("no caches declared, volumes = %s, want only the workspace", mustJSON(t, got))
		}
	})

	t.Run("metadata names the step, sits in the configured namespace and labels the run", func(t *testing.T) {
		job := mustBuild(t, testConfig(), testRun())
		wantLabels := map[string]string{
			"app.kubernetes.io/managed-by": "memql-workbench",
			"memql.io/pipelines-run":       "run-7f3a",
			"memql.io/attempt":             "2",
		}
		wantAnnotations := map[string]string{
			"memql.io/step-key":   "tests/go-tests#2",
			"memql.io/work-run":   "work-91c2",
			"memql.io/owner-user": "user-5d1e",
		}
		meta := job.Metadata
		if meta.Name != testJobName || meta.Namespace != "steps-ns" {
			t.Errorf("metadata = %s/%s, want steps-ns/%s", meta.Namespace, meta.Name, testJobName)
		}
		if !reflect.DeepEqual(meta.Labels, wantLabels) {
			t.Errorf("labels = %v, want %v", meta.Labels, wantLabels)
		}
		if !reflect.DeepEqual(meta.Annotations, wantAnnotations) {
			t.Errorf("annotations = %v, want %v", meta.Annotations, wantAnnotations)
		}
		if got := job.Spec.Template.Metadata.Labels; !reflect.DeepEqual(got, wantLabels) {
			t.Errorf("pod template labels = %v, want the Job's, so a run's pods are selectable too", got)
		}

		run := testRun()
		run.RunID = "v1:pipelines:run:7f3a"
		canonical := mustBuild(t, testConfig(), run)
		if got := canonical.Metadata.Labels["memql.io/pipelines-run"]; got != "r-a52dd336cd341cefd8e6ab57" {
			t.Errorf("run label for a canonical id = %q, want the hashed label value r-a52dd336cd341cefd8e6ab57", got)
		}
	})

	t.Run("a refusal never leaves a half-built Job", func(t *testing.T) {
		type mutation func(cfg *Config, run *StepRun)
		withArtifact := func(path string) mutation {
			return func(_ *Config, run *StepRun) { run.Artifacts = []string{"coverage.out", path} }
		}
		withSecret := func(name string) mutation {
			return func(_ *Config, run *StepRun) { run.Secrets[name] = "planted-" + strings.Repeat("w", 8) }
		}
		cases := []struct {
			name     string
			mutate   mutation
			wantCode string
		}{
			{"no image", func(_ *Config, r *StepRun) { r.Image = "" }, pl.CodeJobRejected},
			{"a blank image", func(_ *Config, r *StepRun) { r.Image = "  " }, pl.CodeJobRejected},
			{"no command", func(_ *Config, r *StepRun) { r.Command = "" }, pl.CodeJobRejected},
			{"a blank command", func(_ *Config, r *StepRun) { r.Command = " \n\t" }, pl.CodeJobRejected},
			{"an absolute artifact path", withArtifact("/etc/passwd"), pl.CodeJobRejected},
			{"an artifact path climbing out", withArtifact("../secrets"), pl.CodeJobRejected},
			{"an artifact path climbing out midway", withArtifact("reports/../../etc"), pl.CodeJobRejected},
			{"an artifact path with a space", withArtifact("my report.xml"), pl.CodeJobRejected},
			{"an artifact path with a tab", withArtifact("a\tb"), pl.CodeJobRejected},
			{"an artifact path with a newline", withArtifact("a\nb"), pl.CodeJobRejected},
			{"an empty artifact path", withArtifact(""), pl.CodeJobRejected},
			{"a secret name starting with a digit", withSecret("1PASSWORD"), pl.CodeJobRejected},
			{"a secret name with a dash", withSecret("MY-TOKEN"), pl.CodeJobRejected},
			{"a secret name with a dot", withSecret("my.token"), pl.CodeJobRejected},
			{"an empty secret name", withSecret(""), pl.CodeJobRejected},
			{"a secret named like a contract variable", withSecret("MEMQL_RUN_ID"), pl.CodeJobRejected},
			{"a secret in the platform's reserved names", withSecret("MEMQL_SHARD"), pl.CodeJobRejected},
			{"a secret named like the wrapper's command", withSecret("MEMQL_STEP_COMMAND"), pl.CodeJobRejected},
			{"a secret named like the artifact marker", withSecret("MEMQL_ARTIFACT_MARKER"), pl.CodeJobRejected},
			{"a secret sharing the clone token's key", withSecret("GIT_TOKEN"), pl.CodeJobRejected},
			{"a secret named like a cache variable", withSecret("GOCACHE"), pl.CodeJobRejected},
			{"a secret also present as a plain value", func(_ *Config, r *StepRun) {
				r.Env["CI"] = "true"
				r.Secrets["CI"] = "planted-" + strings.Repeat("v", 8)
			}, pl.CodeJobRejected},
			{"an unknown cache", func(_ *Config, r *StepRun) { r.Caches = []string{"maven"} }, pl.CodeJobRejected},
			{"no run id", func(_ *Config, r *StepRun) { r.RunID = "" }, pl.CodeJobRejected},
			{"an attempt below one", func(_ *Config, r *StepRun) { r.Attempt = -1 }, pl.CodeJobRejected},
			{"a SHA that is a branch name", func(_ *Config, r *StepRun) { r.SHA = "main" }, pl.CodeJobRejected},
			{"an abbreviated SHA", func(_ *Config, r *StepRun) { r.SHA = "ab12ab1" }, pl.CodeJobRejected},
			{"a SHA that is an option", func(_ *Config, r *StepRun) { r.SHA = "--upload-pack=touch" }, pl.CodeJobRejected},
			{"a clone URL over plain http", func(_ *Config, r *StepRun) { r.Repository.CloneURL = "http://github.com/acme/widget.git" }, pl.CodeJobRejected},
			{"a clone URL in scp form", func(_ *Config, r *StepRun) { r.Repository.CloneURL = "git@github.com:acme/widget.git" }, pl.CodeJobRejected},
			{"a clone URL on the local disk", func(_ *Config, r *StepRun) { r.Repository.CloneURL = "file:///etc" }, pl.CodeJobRejected},
			{"a clone URL that is an option", func(_ *Config, r *StepRun) { r.Repository.CloneURL = "--upload-pack=touch" }, pl.CodeJobRejected},
			{"a clone URL carrying a user", func(_ *Config, r *StepRun) { r.Repository.CloneURL = "https://someone@github.com/acme/widget.git" }, pl.CodeJobRejected},
			{"no clone URL", func(_ *Config, r *StepRun) { r.Repository.CloneURL = "" }, pl.CodeJobRejected},
			{"a service name with capitals", func(_ *Config, r *StepRun) { r.Services["Postgres"] = pl.Service{Image: "postgres:16"} }, pl.CodeJobRejected},
			{"a service name with an underscore", func(_ *Config, r *StepRun) { r.Services["pg_db"] = pl.Service{Image: "postgres:16"} }, pl.CodeJobRejected},
			{"a service name too long for a container", func(_ *Config, r *StepRun) {
				r.Services[strings.Repeat("a", 60)] = pl.Service{Image: "postgres:16"}
			}, pl.CodeJobRejected},
			{"a service with no image", func(_ *Config, r *StepRun) { r.Services["cache"] = pl.Service{} }, pl.CodeJobRejected},
			{"no clone image configured", func(c *Config, _ *StepRun) { c.CloneImage = "" }, pl.CodeRunnerUnavailable},
			{"no TTL configured", func(c *Config, _ *StepRun) { c.JobTTL = 0 }, pl.CodeJobRejected},
		}
		for _, metachar := range []string{";", "&", "|", "$", `\`, "`", "'", `"`, "<", ">"} {
			cases = append(cases, struct {
				name     string
				mutate   mutation
				wantCode string
			}{fmt.Sprintf("an artifact path with %q", metachar), withArtifact("out" + metachar + "x"), pl.CodeJobRejected})
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg, run := testConfig(), testRun()
				tc.mutate(&cfg, &run)
				job, err := BuildJob(cfg, run, testJobName)
				if err == nil {
					t.Fatal("BuildJob succeeded, want a refusal")
				}
				if !reflect.DeepEqual(job, Job{}) {
					t.Errorf("the refusal came with a half-built Job: %s", mustJSON(t, job))
				}
				var refusal *pl.Refusal
				if !errors.As(err, &refusal) {
					t.Fatalf("error %v is not a *pipelines.Refusal, so the runner cannot report a code", err)
				}
				if refusal.Code != tc.wantCode {
					t.Errorf("refusal code = %q, want %q (%v)", refusal.Code, tc.wantCode, err)
				}
				if refusal.Scope != "tests/go-tests#2" {
					t.Errorf("refusal scope = %q, want the step key", refusal.Scope)
				}
			})
		}
	})
}

// TestBuildJobIsAPureFunctionOfItsInputs: a replica rebuilding the Job
// another replica created has to arrive at the same object, so nothing in it
// may follow map iteration order -- not the services, not the environment,
// not the secrets.
func TestBuildJobIsAPureFunctionOfItsInputs(t *testing.T) {
	run := testRun()
	run.Services["cache"] = pl.Service{Image: "memcached:1", Env: map[string]string{"B": "2", "A": "1", "C": "3"}}
	run.Services["queue"] = pl.Service{Image: "nats:2"}
	run.Services["search"] = pl.Service{Image: "opensearch:2"}
	run.Secrets["SENTRY_DSN"] = "planted-" + strings.Repeat("t", 8)
	run.Secrets["AWS_REGION"] = "planted-" + strings.Repeat("s", 8)
	run.Env["MEMQL_MODE"] = "full"
	run.Env["MEMQL_EVENT"] = "push"

	first := mustJSON(t, mustBuild(t, testConfig(), run))
	for i := 0; i < 50; i++ {
		if again := mustJSON(t, mustBuild(t, testConfig(), run)); again != first {
			t.Fatalf("build %d differs from the first:\n%s\nvs\n%s", i+2, again, first)
		}
	}
}

// TestARefusedCloneURLNeverRepeatsItsCredential: a refusal's sentence becomes
// the step's failure text -- in the run record and on the check run -- so a
// clone URL refused for what it carries must not carry it into the refusal.
// A query or a fragment is refused like a user: a clone URL has no use for
// either, and both would sit in the Job as a plain value.
func TestARefusedCloneURLNeverRepeatsItsCredential(t *testing.T) {
	credential := "planted-" + strings.Repeat("q", 10)
	for _, raw := range []string{
		"https://someone:" + credential + "@github.com/acme/widget.git",
		"https://" + credential + "@github.com/acme/widget.git",
		"https://github.com/acme/widget.git?access_token=" + credential,
		"https://github.com/acme/widget.git#" + credential,
		"https:someone:" + credential + "@github.com/acme/widget.git",
		"https://someone:" + credential + "@github.com:no-port/acme/widget.git",
	} {
		run := testRun()
		run.Repository.CloneURL = raw
		job, err := BuildJob(testConfig(), run, testJobName)
		if err == nil || !reflect.DeepEqual(job, Job{}) {
			t.Errorf("clone URL %q built a Job (err %v)", raw, err)
			continue
		}
		if strings.Contains(err.Error(), credential) {
			t.Errorf("the refusal repeats what the URL carried: %v", err)
		}
	}
}

// TestTheCloneTokenReachesOnlyTheCloneContainer: the token can read the
// repository, so it must never be in the environment of the code the
// repository contains. Only the clone container may reference it -- not the
// step, and not a service image somebody else published.
func TestTheCloneTokenReachesOnlyTheCloneContainer(t *testing.T) {
	job := mustBuild(t, testConfig(), testRun())
	var holders []string
	for _, c := range allContainers(job) {
		for _, e := range c.Env {
			byName := e.Name == "GIT_TOKEN"
			byRef := e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Key == "GIT_TOKEN"
			if byName || byRef {
				holders = append(holders, c.Name)
			}
		}
	}
	if !reflect.DeepEqual(holders, []string{"clone"}) {
		t.Errorf("containers reaching GIT_TOKEN = %q, want exactly [clone]", holders)
	}
}

// TestSecretsAreRefsNeverValuesInTheJob: a Job is readable by anything that
// can list Jobs in the namespace and lands in the API server's audit log, so a
// resolved secret travels only inside the Secret, and the Job names it.
func TestSecretsAreRefsNeverValuesInTheJob(t *testing.T) {
	run := testRun()
	job := mustBuild(t, testConfig(), run)
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for name, value := range run.Secrets {
		if strings.Contains(string(raw), value) {
			t.Errorf("the value of secret %s appears in the marshalled Job", name)
		}
	}

	step := stepOf(t, job)
	for name := range run.Secrets {
		e, ok := envNamed(step, name)
		if !ok {
			t.Errorf("secret %s does not reach the step at all", name)
			continue
		}
		want := &EnvVarSource{SecretKeyRef: &SecretKeySelector{Name: testSecretName, Key: name}}
		if e.Value != "" || !reflect.DeepEqual(e.ValueFrom, want) {
			t.Errorf("step env %s = %+v / %+v, want only a secretKeyRef to %s/%s", name, e.Value, e.ValueFrom, testSecretName, name)
		}
	}
}

func TestBuildSecretHoldsTheCloneTokenAndTheStepSecrets(t *testing.T) {
	cfg, run := testConfig(), testRun()

	t.Run("one Secret per Job, labelled with the run so a cancel finds it", func(t *testing.T) {
		sec := BuildSecret(cfg, run, testJobName, plantedClone)
		if sec.APIVersion != "v1" || sec.Kind != "Secret" || sec.Type != "Opaque" {
			t.Errorf("secret = %s/%s type %q, want v1/Secret type Opaque", sec.APIVersion, sec.Kind, sec.Type)
		}
		if sec.Metadata.Name != "mp-a7a72726d5075767e0b6d115-env" || sec.Metadata.Namespace != "steps-ns" {
			t.Errorf("secret = %s/%s, want steps-ns/mp-a7a72726d5075767e0b6d115-env", sec.Metadata.Namespace, sec.Metadata.Name)
		}
		wantLabels := map[string]string{
			"app.kubernetes.io/managed-by": "memql-workbench",
			"memql.io/pipelines-run":       "run-7f3a",
			"memql.io/attempt":             "2",
		}
		if !reflect.DeepEqual(sec.Metadata.Labels, wantLabels) {
			t.Errorf("secret labels = %v, want %v", sec.Metadata.Labels, wantLabels)
		}
		wantData := map[string][]byte{
			"GIT_TOKEN":  []byte(plantedClone),
			"NPM_TOKEN":  []byte(plantedNPM),
			"DEPLOY_KEY": []byte(plantedDeploy),
		}
		if !reflect.DeepEqual(sec.Data, wantData) {
			t.Errorf("secret data keys = %v, want GIT_TOKEN, NPM_TOKEN, DEPLOY_KEY with their values", keysOf(sec.Data))
		}
		doc := wire(t, sec)
		wantField(t, doc, base64.StdEncoding.EncodeToString([]byte(plantedClone)), "data", "GIT_TOKEN")
	})

	t.Run("every reference in the Job names this Secret and a key it holds", func(t *testing.T) {
		sec := BuildSecret(cfg, run, testJobName, plantedClone)
		job := mustBuild(t, cfg, run)
		refs := 0
		for _, c := range allContainers(job) {
			for _, e := range c.Env {
				if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
					continue
				}
				refs++
				ref := e.ValueFrom.SecretKeyRef
				if ref.Name != sec.Metadata.Name {
					t.Errorf("%s/%s references Secret %q, but the Secret is %q", c.Name, e.Name, ref.Name, sec.Metadata.Name)
				}
				if _, ok := sec.Data[ref.Key]; !ok {
					t.Errorf("%s/%s references key %q, which the Secret does not hold", c.Name, e.Name, ref.Key)
				}
			}
		}
		if refs != 3 {
			t.Errorf("%d secret references in the Job, want 3 (the clone token and two step secrets)", refs)
		}
	})

	t.Run("an anonymous clone carries no token key, and the clone's reference tolerates that", func(t *testing.T) {
		sec := BuildSecret(cfg, run, testJobName, "")
		if _, ok := sec.Data["GIT_TOKEN"]; ok {
			t.Error("an anonymous clone's Secret holds a GIT_TOKEN key")
		}
		clone := cloneOf(t, mustBuild(t, cfg, run))
		e, ok := envNamed(clone, "GIT_TOKEN")
		if !ok || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
			e.ValueFrom.SecretKeyRef.Optional == nil || !*e.ValueFrom.SecretKeyRef.Optional {
			t.Errorf("clone GIT_TOKEN = %+v, want an optional reference, or a public clone cannot start", e)
		}
	})

	t.Run("a step secret never shadows the clone token", func(t *testing.T) {
		shadowing := testRun()
		shadowing.Secrets["GIT_TOKEN"] = "planted-" + strings.Repeat("u", 8)
		sec := BuildSecret(cfg, shadowing, testJobName, plantedClone)
		if got := string(sec.Data["GIT_TOKEN"]); got != plantedClone {
			t.Errorf("GIT_TOKEN holds %q, want the clone token: a step secret took the token's place", got)
		}
		if _, err := BuildJob(cfg, shadowing, testJobName); err == nil {
			t.Error("BuildJob accepted a step secret named GIT_TOKEN")
		}
	})
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
