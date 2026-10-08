package pipelinesteps

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func imageBuildRun() (Config, StepRun) {
	cfg := testConfig()
	cfg.ImageBuilder, cfg.NodePool = ImageBuilderRootlessV1, "builds"
	run := testRun()
	run.ImageBuild = &pl.ImageBuild{Context: ".", Dockerfile: "Dockerfile", Args: map[string]string{"BUILD_TAGS": "edge"}}
	run.ImageBuild = imageBuildWithProvenance(run.ImageBuild, run.SHA, pl.Event(run.Env[eventVar]), run.Env["MEMQL_VERSION"])
	run.Image, run.Command, run.ImagePullSecret = "", "", ""
	run.Caches, run.Services, run.Secrets, run.Needs = nil, nil, nil, nil
	run.Platform, run.MemoryMiB = "linux/arm64", 2048
	run.Artifacts = pl.ImageBuildArtifacts()
	return cfg, run
}

func TestImageBuildJobHasOnlyItsBoundedRootlessPrivileges(t *testing.T) {
	cfg, run := imageBuildRun()
	run.Env["ROOTLESSKIT_FLAGS"] = "--net=host"
	job, err := BuildJob(cfg, run, testJobName)
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.HostUsers == nil || *pod.HostUsers || len(pod.ImagePullSecrets) != 0 ||
		len(pod.Containers) != 2 || len(pod.InitContainers) != 2 || pod.FSGroupForTest() != 1000 {
		t.Fatalf("unexpected builder pod authority: %+v", pod)
	}
	producer := pod.Containers[0]
	security := producer.SecurityContext
	if producer.Image != imageBuildImage || security.RunAsUser == nil || *security.RunAsUser != 1000 || security.ProcMount != "Unmasked" ||
		!reflect.DeepEqual(security.Capabilities.Drop, []string{"ALL"}) || !reflect.DeepEqual(security.Capabilities.Add, []string{"SETUID", "SETGID"}) ||
		security.SeccompProfile.Type != "Localhost" || security.SeccompProfile.LocalhostProfile != imageBuildProfile("linux/arm64") {
		t.Fatalf("builder security differs from its fixed profile: %+v", producer)
	}
	for _, v := range producer.Env {
		if v.ValueFrom != nil || v.Name == "ROOTLESSKIT_FLAGS" || v.Name == gitTokenKey {
			t.Fatalf("builder received uncontrolled environment: %+v", v)
		}
		if v.Name == "BUILDKITD_FLAGS" && strings.Contains(v.Value, "no-process-sandbox") {
			t.Fatalf("builder still shares the daemon PID namespace: %q", v.Value)
		}
	}
	wire, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(wire, &document); err != nil {
		t.Fatal(err)
	}
	spec := document["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if hostUsers, ok := spec["hostUsers"].(bool); !ok || hostUsers {
		t.Fatalf("typed image-build pod did not serialize hostUsers:false: %s", wire)
	}
	containers := spec["containers"].([]any)
	securityWire := containers[0].(map[string]any)["securityContext"].(map[string]any)
	if securityWire["procMount"] != "Unmasked" {
		t.Fatalf("typed image-build pod did not serialize procMount:Unmasked: %s", wire)
	}
	for _, v := range pod.Volumes {
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit != cfg.WorkspaceLimit {
			t.Fatalf("builder received shared or unbounded storage: %+v", v)
		}
	}
	if producer.Resources.Limits["cpu"] != "2" || producer.Resources.Limits["memory"] != "2048Mi" || producer.Resources.Limits["ephemeral-storage"] != cfg.WorkspaceLimit {
		t.Fatal("builder lost its resource bounds")
	}
	for _, c := range append(pod.InitContainers, pod.Containers[1:]...) {
		if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation || len(c.SecurityContext.Capabilities.Add) > 0 {
			t.Fatalf("builder privilege leaked to %s", c.Name)
		}
	}
	ordinary, err := BuildJob(testConfig(), testRun(), testJobName)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Spec.Template.Spec.HostUsers != nil {
		t.Fatal("ordinary command Job unexpectedly opted into a user namespace")
	}
	for _, container := range ordinary.Spec.Template.Spec.Containers {
		if container.SecurityContext.ProcMount != "" {
			t.Fatalf("ordinary command container unexpectedly requested procMount=%q", container.SecurityContext.ProcMount)
		}
	}
}

func (p PodSpec) FSGroupForTest() int64 {
	if p.SecurityContext == nil || p.SecurityContext.FSGroup == nil {
		return -1
	}
	return *p.SecurityContext.FSGroup
}

func TestImageBuildCPUReservationHonorsOperatorBudget(t *testing.T) {
	cfg, run := imageBuildRun()
	run.CPUMilli = 4000
	job, err := BuildJob(cfg, run, testJobName)
	if err != nil {
		t.Fatal(err)
	}
	producer := job.Spec.Template.Spec.Containers[0]
	if producer.Resources.Requests["cpu"] != "4000m" || producer.Resources.Limits["cpu"] != "4000m" || producer.Resources.Limits["memory"] != "2048Mi" {
		t.Fatal("typed build replaced an explicit resource reservation", producer.Resources)
	}
	if producer.SecurityContext.SeccompProfile.LocalhostProfile != imageBuildProfile("linux/arm64") {
		t.Fatal("resource reservation changed the fixed builder security profile")
	}
	for _, container := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers[1:]...) {
		if container.Resources != nil && container.Resources.Limits["cpu"] == "4000m" {
			t.Fatal("producer CPU reservation leaked to another container")
		}
	}
	cfg.StepCPUMaxMilli = 2000
	if refused, err := BuildJob(cfg, run, testJobName); err == nil || refused.Kind != "" {
		t.Fatal("typed build bypassed operator CPU cap")
	}
}

func TestImageBuildRefusesUnsafeForwardBeforeResources(t *testing.T) {
	for name, edit := range map[string]func(*Config, *StepRun){
		"disabled":                    func(c *Config, r *StepRun) { c.ImageBuilder = "" },
		"unknown profile":             func(c *Config, r *StepRun) { c.ImageBuilder = "privileged" },
		"shared serving pool":         func(c *Config, r *StepRun) { c.NodePool = "" },
		"credential":                  func(c *Config, r *StepRun) { r.Secrets = map[string]string{"TOKEN": "secret"} },
		"services":                    func(c *Config, r *StepRun) { r.Services = map[string]pl.Service{"db": {Image: "db"}} },
		"shared cache":                func(c *Config, r *StepRun) { r.Caches = []string{"go"} },
		"command":                     func(c *Config, r *StepRun) { r.Command = "override" },
		"image":                       func(c *Config, r *StepRun) { r.Image = "override" },
		"artifact":                    func(c *Config, r *StepRun) { r.Artifacts = []string{"other"} },
		"no memory bound":             func(c *Config, r *StepRun) { r.MemoryMiB = 0 },
		"forged commit":               func(c *Config, r *StepRun) { r.ImageBuild.Args["MEMQL_COMMIT"] = "another-source" },
		"missing release metadata":    func(c *Config, r *StepRun) { delete(r.ImageBuild.Args, "MEMQL_RELEASE") },
		"environment source mismatch": func(c *Config, r *StepRun) { r.Env["MEMQL_SHA"] = "another-source" },
		"forged release metadata":     func(c *Config, r *StepRun) { r.ImageBuild.Args["MEMQL_RELEASE"] = "9.9.9" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, run := imageBuildRun()
			edit(&cfg, &run)
			if job, err := BuildJob(cfg, run, testJobName); err == nil || job.Kind != "" {
				t.Fatalf("unsafe Job built: %+v %v", job, err)
			}
		})
	}
}

func TestImageBuildArgumentsRemainLiteralShellWords(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "buildctl-daemonless.sh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	value := "quote' newline\n$(touch injected); --secret=id=publish"
	build := imageBuildWithProvenance(&pl.ImageBuild{Context: ".", Dockerfile: "Dockerfile", Args: map[string]string{"VALUE": value}}, testSHA, pl.EventRelease, "v1.2.3")
	// Move the fixed checkout mount to isolated test scratch; everything else
	// is the real generated program, executed by the real POSIX shell.
	program := strings.ReplaceAll(imageBuildCommand(build, "linux/arm64"), workspacePath, root)
	cmd := exec.Command("/bin/sh", "-ec", program)
	cmd.Dir, cmd.Env = root, append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("builder arguments: %v %s", err, out)
	}
	if !strings.Contains(string(out), "build-arg:VALUE="+value+"\n") {
		t.Fatalf("argument bytes changed: %s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); !os.IsNotExist(err) {
		t.Fatal("build argument executed as shell code")
	}
	if !strings.Contains(string(out), "build-arg:MEMQL_COMMIT="+testSHA+"\n") || !strings.Contains(string(out), "build-arg:MEMQL_RELEASE=1.2.3\n") {
		t.Fatalf("pinned provenance was not passed to BuildKit: %s", out)
	}
}
