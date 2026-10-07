package pipelinesteps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
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
