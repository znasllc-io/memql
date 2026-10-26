package pipelinesteps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// The process fixture checks the actual cloud shell boundary and full-export
// arguments, not BuildKit isolation. Real Dockerfile execution is a separate
// Linux qualification prerequisite.
func TestImageVerificationCloudCommandRequiresFullSuccessfulExport(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete export", true: "failed export"}[failed], func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			fake := `#!/bin/sh
set -eu
printf '%s\n' "$@" > argv.txt
printf 'genuine fixture export' > .memql-image-build/image.oci.tar
printf '{"containerimage.digest":"sha256:fixture"}' > .memql-image-build/metadata.json
test "${FAKE_FAIL:-0}" = 0
`
			if err := os.WriteFile(filepath.Join(bin, "buildctl-daemonless.sh"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := exec.LookPath("sha256sum"); err != nil {
				if err := os.WriteFile(filepath.Join(bin, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, run := imageBuildRun()
			run.ImageBuild.VerificationOnly = true
			run.ImageBuild.Target = "verified"
			command := strings.ReplaceAll(imageBuildCommand(run.ImageBuild, run.Platform, false), workspacePath, dir)
			cmd := exec.Command("sh", "-ec", command)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			if failed {
				cmd.Env = append(cmd.Env, "FAKE_FAIL=1")
			}
			out, err := cmd.CombinedOutput()
			if failed {
				if err == nil {
					t.Fatal("failed export succeeded")
				}
				if _, e := os.Stat(filepath.Join(dir, pl.ImageVerificationPath)); !os.IsNotExist(e) {
					t.Fatal("failed export left a success receipt", e)
				}
				return
			}
			if err != nil {
				t.Fatalf("command: %v: %s", err, out)
			}
			argv, _ := os.ReadFile(filepath.Join(dir, "argv.txt"))
			if !strings.Contains(string(argv), "type=oci,dest=") || !strings.Contains(string(argv), "target=verified") {
				t.Fatalf("target/export skipped: %s", argv)
			}
			raw, err := os.ReadFile(filepath.Join(dir, pl.ImageVerificationPath))
			if err != nil {
				t.Fatal(err)
			}
			if err := pl.ValidateImageVerificationReceipt(raw, run.SHA, run.Platform, run.ImageBuild); err != nil {
				t.Fatalf("receipt: %v: %s", err, raw)
			}
			for _, name := range []string{"image.oci.tar", "metadata.json"} {
				if _, err := os.Stat(filepath.Join(dir, pl.ImageBuildDirectory, name)); !os.IsNotExist(err) {
					t.Fatal("throwaway image retained", name, err)
				}
			}
		})
	}
}

func TestImageVerificationCloudJobRetainsIsolationAndRefusesCredential(t *testing.T) {
	cfg, run := imageBuildRun()
	run.ImageBuild.VerificationOnly = true
	run.Artifacts = pl.ImageBuildArtifactsFor(run.ImageBuild)
	job, err := BuildJob(cfg, run, testJobName)
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.HostUsers == nil || *job.Spec.Template.Spec.HostUsers || job.Spec.Template.Spec.Containers[0].Image != imageBuildImage {
		t.Fatal("verification weakened image isolation")
	}
	run.GitHubReadToken = true
	if _, err := BuildJob(cfg, run, testJobName); err == nil {
		t.Fatal("verification received checkout credential")
	}
}

func TestImageVerificationReceiptIsValidatedBeforeArtifactStorage(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "wrong source"}[invalid], func(t *testing.T) {
			h := newRunnerHarness(t)
			_, run := imageBuildRun()
			run.ImageBuild.VerificationOnly = true
			run.Artifacts = pl.ImageBuildArtifactsFor(run.ImageBuild)
			receipt := pl.ImageVerificationReceipt{SchemaVersion: 1, Kind: "image-verification", SourceSHA: run.SHA, Platform: run.Platform, Context: run.ImageBuild.Context, Dockerfile: run.ImageBuild.Dockerfile, Target: run.ImageBuild.Target, Args: run.ImageBuild.Args, SuccessfulBuild: true, RuntimeProfile: ImageBuilderRootlessV1, OCISHA256: strings.Repeat("a", 64), OCIBytes: 256, BuildMetadata: json.RawMessage(`{}`)}
			if invalid {
				receipt.SourceSHA = strings.Repeat("b", 40)
			}
			raw, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "evidence.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			snapshot := &ArtifactSnapshot{Files: []SnapshotFile{{Path: pl.ImageVerificationPath, Size: int64(len(raw)), SHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), local: path}}}
			files := stepFiles{run: run, library: h.lib, log: h.r.log, mask: func(s string) string { return s }}
			var result pl.StepResult
			notes := &noteList{}
			err = files.storeArtifactSnapshot(context.Background(), snapshot, &result, notes)
			if invalid {
				if err == nil || len(h.lib.stored()) != 0 {
					t.Fatal("unbound receipt stored", err)
				}
			} else if err != nil || len(result.ArtifactIntentIDs) != 1 {
				t.Fatal("valid verification evidence lost", err, result)
			}
		})
	}
}
