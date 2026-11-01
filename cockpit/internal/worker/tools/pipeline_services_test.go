package tools

import (
	"context"
	"encoding/json"
	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPipelineServicesValidateBeforeClone(t *testing.T) {
	service := func(extra map[string]any) map[string]any {
		out := map[string]any{"image": containerTestDigest}
		for key, value := range extra {
			out[key] = value
		}
		return out
	}
	for _, raw := range []any{nil, []any{}, "postgres", map[string]any{"Bad": service(nil)},
		map[string]any{"db": service(map[string]any{"image": "postgres:16"})},
		map[string]any{"db": service(map[string]any{"ready": true})},
		map[string]any{"db": service(map[string]any{"env": map[string]any{"BAD KEY": "x"}})},
		map[string]any{"db": service(map[string]any{"ports": []any{5432}})},
		map[string]any{"a": service(nil), "b": service(nil), "c": service(nil), "d": service(nil), "e": service(nil)},
	} {
		req := pipelineStepRequest{execution: "container"}
		if err := parsePipelineServices(map[string]any{"services": raw}, &req); err == nil {
			t.Fatalf("accepted %v", raw)
		}
	}
	valid := map[string]any{"services": map[string]any{"db": service(map[string]any{"ready": "pg_isready", "env": map[string]any{"PGDATA": "/db"}})}}
	req := pipelineStepRequest{execution: "native"}
	if err := parsePipelineServices(valid, &req); err == nil {
		t.Fatal("native services accepted")
	}
	req.execution = "container"
	if err := parsePipelineServices(valid, &req); err != nil || len(req.services) != 1 || req.services[0].env["PGDATA"] != "/db" {
		t.Fatalf("valid service: %+v %v", req, err)
	}
}

func TestPipelineServiceFailureCleansEveryPlannedResource(t *testing.T) {
	for _, failAt := range []string{"network", "create", "start", "readiness", "cleanup"} {
		t.Run(failAt, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls")
			fakePipelineDocker(t, `printf '%s\n' "$*" >> `+shellLiteral(log)+`
case "$1" in
network) if [ "$2" = create ] && [ `+shellLiteral(failAt)+` = network ]; then exit 1; fi; exit 0;;
info) echo daemon-id;;
create) [ `+shellLiteral(failAt)+` != create ]; exit $?;;
image) echo linux/arm64;;
start) [ `+shellLiteral(failAt)+` != start ]; exit $?;;
inspect) printf '%s' '{"Running":true,"Health":{"Status":"unhealthy"}}';;
rm|ps) [ `+shellLiteral(failAt)+` != cleanup ]; exit $?;;
logs) exit 0;;
*) exit 9;;
esac`)
			reservation := testPipelineReservation(t)
			run := &pipelineRun{reservation: reservation, dir: pipelineTestWorkspace(t), req: pipelineStepRequest{execution: "container", dockerID: "daemon-id", dockerEndpoint: "unix:///tmp/memql-pipeline-test.sock", platform: "linux/arm64", image: containerTestDigest, services: []pipelineService{{name: "db", image: containerTestDigest, ready: "false"}, {name: "other", image: containerTestDigest}}}, mask: newSecretMasker(nil), emit: func(bool, []byte) {}}
			_, fail := run.containerCommand(context.Background())
			if fail == nil {
				t.Fatal("service failure became success")
			}
			if err := finishPipelineWorkspace(reservation, run.dir); err != nil {
				t.Fatal(err)
			}
			if (failAt == "cleanup") != reservation.dirty {
				t.Fatalf("dirty=%v after %s", reservation.dirty, failAt)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{"rm --force --volumes memql-step-", "-svc-0", "-svc-1", "network rm memql-step-"} {
				if !strings.Contains(string(calls), required) {
					t.Errorf("cleanup omitted %s: %s", required, calls)
				}
			}
			if strings.Contains(string(calls), "start --attach") {
				t.Fatal("command started before services were ready")
			}
		})
	}
}

func TestPipelineServiceRecoveryRejectsUnrelatedResourceNames(t *testing.T) {
	name := "memql-step-" + strings.Repeat("a", 32)
	record := pipelineAttemptRecord{Execution: "container", DockerID: "daemon", DockerEndpoint: "unix:///tmp/memql-pipeline-test.sock", Container: name, Network: name + "-net", Services: []string{name + "-svc-0"}}
	if !validPipelineAttempt(record) {
		t.Fatal("valid record refused")
	}
	for _, mutate := range []func(*pipelineAttemptRecord){
		func(r *pipelineAttemptRecord) { r.Network = "someone-elses-network" },
		func(r *pipelineAttemptRecord) { r.Services = []string{"someone-elses-container"} },
		func(r *pipelineAttemptRecord) { r.Services = []string{name + "-svc-1"} },
		func(r *pipelineAttemptRecord) { r.Services = nil },
	} {
		bad := record
		mutate(&bad)
		if validPipelineAttempt(bad) {
			t.Fatalf("unsafe recovery accepted %+v", bad)
		}
	}
}

func TestPipelineServicesRealDocker(t *testing.T) {
	image := os.Getenv("MEMQL_TEST_PIPELINE_POSTGRES_IMAGE")
	if image == "" {
		t.Skip("set MEMQL_TEST_PIPELINE_POSTGRES_IMAGE to a pinned PostgreSQL image to require service tests")
	}
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	policy, root := pipelineTestPolicy(t, "")
	pipelineHostMemory = capacity.ReadSystemHeadroom
	pipelineHostCPUs = runtime.NumCPU
	extra := map[string]any{"execution": "container", "platform": "linux/" + runtime.GOARCH, "image": image,
		"services": map[string]any{"postgres": map[string]any{"image": image, "env": map[string]any{"POSTGRES_USER": "memql", "POSTGRES_PASSWORD": "fixture-password", "POSTGRES_DB": "memql"}, "ready": "pg_isready -h localhost -U memql"}},
		"env":      map[string]any{"PGPASSWORD": "fixture-password"}, "secrets": map[string]any{"STEP_ONLY_SECRET": "must-never-reach-service"}}
	var output recordedOutput
	command := `psql -h 127.0.0.1 -U memql -d memql -Atc 'SELECT 42' > result.txt && test "$(cat result.txt)" = 42 && test "$(cat file.txt)" = second`
	extra["artifacts"] = []string{"result.txt"}
	result := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx, command, extra), policy, output.emit, output.artifact))
	if result.ExitCode != 0 {
		t.Fatalf("database command: %d %s %s", result.ExitCode, output.text(true), output.text(false))
	}
	if files := untarGz(t, output.artifactBytes()); files["result.txt"] != "42\n" {
		t.Fatalf("artifact: %v", files)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("workspace: %v %v", entries, err)
	}

	// Cancel while the build is using its ready database. Inspect the LIVE
	// service before cancellation, including the absence of step credentials,
	// workspace/socket mounts, published ports and elevated capabilities.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelled recordedOutput
	var attempt pipelineAttemptRecord
	var inspectionErr error
	cancelled.onChunk = func(c recordedChunk) {
		if !strings.Contains(c.data, "database-ready-for-cancel") {
			return
		}
		capacity, _ := pipelineCapacityRoot()
		data, err := os.ReadFile(filepath.Join(capacity, "pipeline.lock"))
		if err != nil {
			inspectionErr = err
			cancel()
			return
		}
		if err = json.Unmarshal(data, &attempt); err != nil {
			inspectionErr = err
			cancel()
			return
		}
		data, err = dockerOutput(withPipelineDockerEndpoint(context.Background(), attempt.DockerEndpoint), "inspect", attempt.Services[0])
		if err != nil {
			inspectionErr = err
			cancel()
			return
		}
		var containers []struct {
			Config     struct{ Env []string }
			HostConfig struct {
				Privileged   bool
				Binds        []string
				PortBindings map[string]any
				CapDrop      []string
				NetworkMode  string
			}
			Mounts []struct{ Type, Destination string }
		}
		if err = json.Unmarshal(data, &containers); err != nil {
			inspectionErr = err
			cancel()
			return
		}
		if len(containers) != 1 {
			t.Error("missing service inspect")
		} else {
			svc := containers[0]
			for _, value := range svc.Config.Env {
				if strings.Contains(value, "must-never-reach-service") || strings.HasPrefix(value, workerTokenVariable+"=") {
					t.Error("step/worker secret reached service")
				}
			}
			if svc.HostConfig.Privileged || len(svc.HostConfig.Binds) != 0 || len(svc.HostConfig.PortBindings) != 0 || (!reflect.DeepEqual(svc.HostConfig.CapDrop, []string{"NET_RAW"}) && !reflect.DeepEqual(svc.HostConfig.CapDrop, []string{"CAP_NET_RAW"})) || svc.HostConfig.NetworkMode != attempt.Network {
				t.Errorf("unsafe service config: %+v", svc.HostConfig)
			}
			for _, mount := range svc.Mounts {
				if mount.Type == "bind" || mount.Destination == "/workspace" || mount.Destination == "/var/run/docker.sock" {
					t.Errorf("unsafe mount: %+v", mount)
				}
			}
		}
		cancel()
	}
	began := time.Now()
	_, fail := runPipelineStep(ctx, "", stepArgs(t, fx, "echo database-ready-for-cancel; sleep 300", extra), policy, cancelled.emit, cancelled.artifact)
	if inspectionErr != nil {
		t.Fatal(inspectionErr)
	}
	if fail == nil || fail.GetErrorCode() != "cancelled" || time.Since(began) > 45*time.Second {
		t.Fatalf("cancellation: %v after %s", fail, time.Since(began))
	}
	if !validPipelineAttempt(attempt) {
		t.Fatal("did not inspect the running service")
	}
	dockerCtx := withPipelineDockerEndpoint(context.Background(), attempt.DockerEndpoint)
	for _, name := range append(attempt.Services, attempt.Container) {
		out, err := dockerOutput(dockerCtx, "ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Fatalf("container remains %s: %s %v", name, out, err)
		}
	}
	out, err := dockerOutput(dockerCtx, "network", "ls", "--filter", "name=^"+attempt.Network+"$", "--format", "{{.Name}}")
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("network remains: %s %v", out, err)
	}

	// Recreate an interrupted attempt with two services and a command, then
	// release the kernel lock as a dead worker would. A fresh reservation must
	// reconcile every recorded resource before another build may use capacity.
	orphan, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.close()
	attempt.Services = append(attempt.Services, attempt.Container+"-svc-1")
	attempt.Workspace, err = os.MkdirTemp(root, "step-")
	if err != nil {
		t.Fatal(err)
	}
	if err = orphan.begin(attempt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = removePipelineAttempt(cleanup, attempt)
	})
	if _, err = dockerOutput(dockerCtx, "network", "create", attempt.Network); err != nil {
		t.Fatal(err)
	}
	for i, name := range append(attempt.Services, attempt.Container) {
		network := "container:" + attempt.Services[0]
		if i == 0 {
			network = attempt.Network
		}
		if _, err = dockerOutput(dockerCtx, "create", "--name", name, "--network", network, "--entrypoint=/bin/sh", image, "-c", "sleep 300"); err != nil {
			t.Fatal(err)
		}
		if _, err = dockerOutput(dockerCtx, "start", name); err != nil {
			t.Fatal(err)
		}
	}
	orphan.close()
	replacement, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.close()
	if !replacement.dirty {
		t.Fatal("crashed worker lost the attempt record")
	}
	if err = reconcilePipelineReservation(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.dirty {
		t.Fatal("reconciled attempt retained capacity")
	}
	for _, name := range append(attempt.Services, attempt.Container) {
		out, err := dockerOutput(dockerCtx, "ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Fatalf("orphan remains %s: %s %v", name, out, err)
		}
	}
	out, err = dockerOutput(dockerCtx, "network", "ls", "--filter", "name=^"+attempt.Network+"$", "--format", "{{.Name}}")
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("orphan network remains: %s %v", out, err)
	}
	if _, err = os.Stat(attempt.Workspace); !os.IsNotExist(err) {
		t.Fatal("orphan workspace retained")
	}
}
