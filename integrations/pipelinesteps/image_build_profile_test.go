package pipelinesteps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageBuildProfilesPinTheirExactBytesAndDenyProcessInspection(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "components", "pipeline-image-builder", ImageBuilderRootlessV1+"-"+arch+".json"))
			if err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(body))
			if !strings.HasSuffix(imageBuildProfile("linux/"+arch), "-"+digest+".json") {
				t.Fatal("profile bytes differ from the runner's pinned path")
			}
			var profile struct {
				DefaultAction string
				Syscalls      []struct {
					Action string
					Names  []string
				}
			}
			if err := json.Unmarshal(body, &profile); err != nil {
				t.Fatal(err)
			}
			if profile.DefaultAction != "SCMP_ACT_ERRNO" {
				t.Fatal("profile must deny unspecified syscalls")
			}
			for _, rule := range profile.Syscalls {
				for _, name := range rule.Names {
					switch name {
					case "ptrace", "process_vm_readv", "process_vm_writev", "bpf", "perf_event_open", "open_by_handle_at", "userfaultfd", "kexec_load":
						if rule.Action == "SCMP_ACT_ALLOW" {
							t.Fatalf("profile grants %s", name)
						}
					}
				}
			}
		})
	}
}

func TestImageBuildAppArmorProfilePinsItsExactBytes(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "components", "pipeline-image-builder", ImageBuilderRootlessV1+".apparmor"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("memql-rootless-buildkit-v1-%x", sha256.Sum256(body)); imageBuildAppArmorProfile != want {
		t.Fatalf("AppArmor profile bytes differ from the runner's pinned name: want %s", want)
	}
}

func TestImageBuildProbeCompilesForSupportedArchitectures(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(t.TempDir(), "probe"), "./testdata/image-build/probe.go")
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("probe must compile before cluster qualification: %v\n%s", err, out)
			}
		})
	}
}
