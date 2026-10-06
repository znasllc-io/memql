package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestToolchainBuildAndSmokeUseTheRequestedNativePlatform(t *testing.T) {
	raw := toolchainRead(t, toolchainWorkflowPath)
	var spec struct {
		On map[string]struct {
			Inputs map[string]struct {
				Type, Default string
				Required      bool
				Options       []string
			}
		} `yaml:"on"`
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	platform := spec.On["workflow_dispatch"].Inputs["platform"]
	if platform.Type != "choice" || !platform.Required || platform.Default != "linux/amd64" || !reflect.DeepEqual(platform.Options, []string{"linux/amd64", "linux/arm64"}) {
		t.Fatalf("native platform choice: %+v", platform)
	}
	if got := spec.Jobs["build"].RunsOn; got != "${{ inputs.platform == 'linux/arm64' && 'ubuntu-24.04-arm' || 'ubuntu-24.04' }}" {
		t.Fatalf("runner not bound to native platform: %q", got)
	}
	wf := toolchainLoad(t)
	job, _ := toolchainPushJob(wf)
	for _, step := range job.Steps {
		if step.action() == "docker/build-push-action" && step.with("platforms") != "${{ inputs.platform }}" {
			t.Fatalf("build does not use the requested platform: %s", step.Name)
		}
		if strings.Contains(step.Uses, "qemu") {
			t.Fatal("toolchain smoke test must execute natively")
		}
	}
	first := job.Steps[0]
	if first.Env["PLATFORM"] != "${{ inputs.platform }}" {
		t.Fatal("dispatch guard cannot verify requested platform")
	}
	for _, tc := range []struct {
		platform, arch, ref string
		ok                  bool
	}{
		{"linux/amd64", "x86_64", "refs/heads/main", true},
		{"linux/arm64", "aarch64", "refs/heads/main", true},
		{"linux/arm64", "x86_64", "refs/heads/main", false},
		{"linux/amd64", "aarch64", "refs/heads/main", false},
		{"linux/ppc64le", "ppc64le", "refs/heads/main", false},
		{"linux/arm64", "aarch64", "refs/heads/feature", false},
	} {
		t.Run(tc.platform+"-"+tc.arch+"-"+tc.ref, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "uname"), []byte("#!/bin/sh\nmain() { printf '%s\\n' \"$TEST_ARCH\"; }\nmain \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-eu", "-o", "pipefail", "-c", first.Run)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_ARCH="+tc.arch, "PLATFORM="+tc.platform, "REF="+tc.ref, "VERSION=fixture")
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("guard passed=%v want=%v: %s", err == nil, tc.ok, out)
			}
		})
	}
}

func TestToolchainImmutabilityDistinguishesMissingTagFromRegistryFailure(t *testing.T) {
	job, _ := toolchainPushJob(toolchainLoad(t))
	var guard string
	for _, step := range job.Steps {
		if step.Name == "Immutability guard" {
			guard = step.Run
		}
	}
	if guard == "" {
		t.Fatal("missing guard")
	}
	for _, tc := range []struct {
		name, message, status string
		ok                    bool
	}{
		{"exists", "manifest digest", "0", false},
		{"missing", "ERROR: ghcr.io/org/image:fixture: not found", "1", true},
		{"unknown manifest", "manifest unknown", "1", true},
		{"unauthorized", "unauthorized: authentication required", "1", false},
		{"network", "TLS handshake timeout", "1", false},
		{"server", "503 Service Unavailable", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nmain() { printf '%s\\n' \"$TEST_MESSAGE\"; exit \"$TEST_STATUS\"; }\nmain \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-eu", "-o", "pipefail", "-c", guard)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "IMAGE=ghcr.io/org/image", "VERSION=fixture", "TEST_MESSAGE="+tc.message, "TEST_STATUS="+tc.status)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("guard passed=%v want=%v: %s", err == nil, tc.ok, out)
			}
		})
	}
}
