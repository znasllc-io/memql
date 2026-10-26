package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/imageverify"
)

func imageVerificationArgs() map[string]any {
	sha := strings.Repeat("a", 40)
	return map[string]any{"repository": "owner/repo", "cloneUrl": "https://github.com/owner/repo.git", "sha": sha, "execution": "container", "platform": "linux/amd64", "cpuMilli": 1000, "memoryMiB": 1024, "imageBuild": map[string]any{"context": ".", "dockerfile": "Dockerfile", "target": "verify", "verificationOnly": true, "args": map[string]string{"MEMQL_COMMIT": sha, "MEMQL_RELEASE": ""}}, "artifacts": []any{imageverify.Artifact}}
}
func TestImageVerificationWireRefusesHostAndPublicationInputs(t *testing.T) {
	if _, err := parsePipelineStep(imageVerificationArgs()); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]any{"command": "touch /tmp/forbidden", "execution": "native", "platform": "darwin/arm64", "image": containerTestDigest, "services": []any{"db"}, "needs": []any{"docker"}, "caches": []any{"go"}, "secrets": map[string]string{"SECRET": "value"}, "githubReadToken": "credential", "fullHistory": true, "artifacts": []any{".memql-image-build/image.oci.tar"}} {
		t.Run(key, func(t *testing.T) {
			args := imageVerificationArgs()
			args[key] = value
			if _, err := parsePipelineStep(args); err == nil {
				t.Fatal("forbidden image verification input accepted")
			}
		})
	}
}
func TestImageVerificationKeepsMachineResourceAndConsentLimits(t *testing.T) {
	q, err := parsePipelineStep(imageVerificationArgs())
	if err != nil {
		t.Fatal(err)
	}
	if err = admitPipelineResourceLimits(&q, PipelinesPolicy{ContainerCPUMilli: 500, ContainerMemoryMiB: 1024}); err == nil {
		t.Fatal("cluster enlarged machine CPU allowance")
	}
	// A descriptor alone grants no consent. With no pipeline policy it remains
	// absent, even on a host that could otherwise support the fixed runtime.
	raw, err := CapabilityDescriptorJSONForPipelines(PipelinesPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["pipelineImageVerification"]; ok {
		t.Fatal("unconsented image capability advertised")
	}
}
func TestImageVerificationCleanupUncertaintyKeepsDurableLease(t *testing.T) {
	for _, uncertain := range []bool{true, false} {
		t.Run(map[bool]string{true: "uncertain", false: "confirmed"}[uncertain], func(t *testing.T) {
			reservation := testPipelineReservation(t)
			defer reservation.close()
			workspace, err := os.MkdirTemp(t.TempDir(), "step-")
			if err != nil {
				t.Fatal(err)
			}
			req, err := parsePipelineStep(imageVerificationArgs())
			if err != nil {
				t.Fatal(err)
			}
			req.imageMinFreeBytes = 1 << 30
			before := runImageVerification
			t.Cleanup(func() { runImageVerification = before })
			initial := imageverify.Record{ID: strings.Repeat("b", 32), Workspace: workspace, Root: "/fixed/attempt", AttemptSHA256: strings.Repeat("c", 64), SourceSHA: req.sha, Identity: imageverify.HelloIdentity{InvocationID: strings.Repeat("d", 32)}}
			runImageVerification = func(_ context.Context, _ string, _ string, _ imageverify.Request, _ imageverify.Limits, persist imageverify.Persist, _ imageverify.Emit) error {
				if err := persist(initial); err != nil {
					return err
				}
				if uncertain {
					return imageverify.ErrCleanup
				}
				return errors.New("build failed but runtime cleanup confirmed")
			}
			run := pipelineRun{req: req, dir: workspace, reservation: reservation, mask: &secretMasker{}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, failure := run.imageVerification(ctx)
			if failure == nil {
				t.Fatal("failed build succeeded")
			}
			retained, err := reservation.read()
			if err != nil {
				t.Fatal(err)
			}
			if uncertain {
				if failure.ErrorCode != "pipeline_cleanup_uncertain" || retained.ImageVerification == nil || retained.Execution != "image-verification" {
					t.Fatal("uncertain effects were cleared")
				}
			} else if retained.Execution != "cleanup" || retained.ImageVerification != nil {
				t.Fatal("confirmed cleanup did not permit ordinary workspace teardown")
			}
		})
	}
}
func TestImageVerificationRecoveryUpdatesOnlyInvocation(t *testing.T) {
	reservation := testPipelineReservation(t)
	defer reservation.close()
	workspace := filepath.Join(t.TempDir(), "step-exact")
	original := imageverify.Record{ID: strings.Repeat("a", 32), Workspace: workspace, Root: "/fixed/attempt", AttemptSHA256: strings.Repeat("b", 64), SourceSHA: strings.Repeat("c", 40), Identity: imageverify.HelloIdentity{InvocationID: strings.Repeat("d", 32)}}
	if err := reservation.begin(pipelineAttemptRecord{Execution: "image-verification", Workspace: workspace, ImageVerification: &original}); err != nil {
		t.Fatal(err)
	}
	next := original
	next.Identity.InvocationID = strings.Repeat("e", 32)
	if err := reservation.updateImageVerification(next); err != nil {
		t.Fatal(err)
	}
	for label, change := range map[string]func(*imageverify.Record){"source": func(r *imageverify.Record) { r.SourceSHA = strings.Repeat("f", 40) }, "marker": func(r *imageverify.Record) { r.AttemptSHA256 = strings.Repeat("f", 64) }, "root": func(r *imageverify.Record) { r.Root = "/foreign" }, "workspace": func(r *imageverify.Record) { r.Workspace = "/foreign" }} {
		t.Run(label, func(t *testing.T) {
			wrong := next
			change(&wrong)
			if reservation.updateImageVerification(wrong) == nil {
				t.Fatal("recovery changed original attempt binding")
			}
		})
	}
	got, err := reservation.read()
	if err != nil || got.ImageVerification == nil || got.ImageVerification.AttemptSHA256 != original.AttemptSHA256 || got.ImageVerification.Identity.InvocationID != next.Identity.InvocationID {
		t.Fatal("recovery did not retain exact attempt")
	}
}
