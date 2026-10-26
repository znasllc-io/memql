package tools

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
)

func fakePipelineHostHeadroom(t *testing.T) {
	t.Helper()
	priorMemory, priorCPU := pipelineHostMemory, pipelineHostCPUs
	pipelineHostMemory = func() (capacity.SystemHeadroom, error) {
		return capacity.SystemHeadroom{Total: 64 << 30, Available: 48 << 30}, nil
	}
	pipelineHostCPUs = func() int { return 32 }
	t.Cleanup(func() { pipelineHostMemory, pipelineHostCPUs = priorMemory, priorCPU })
}

func TestPipelineResourceWireAndOwnerCeilings(t *testing.T) {
	for _, bad := range []any{nil, "2000", true, -1.0, 0.5, math.NaN(), math.Inf(1), 256001.0} {
		req := pipelineStepRequest{execution: "container"}
		if err := parsePipelineResources(map[string]any{"cpuMilli": bad}, &req); err == nil {
			t.Fatalf("accepted CPU %v", bad)
		}
	}
	for _, bad := range []any{nil, "2048", true, -1.0, 127.0, 128.5, 1048577.0} {
		req := pipelineStepRequest{execution: "container"}
		if err := parsePipelineResources(map[string]any{"memoryMiB": bad}, &req); err == nil {
			t.Fatalf("accepted memory %v", bad)
		}
	}
	for _, field := range []string{"cpuMilli", "memoryMiB"} {
		req := pipelineStepRequest{execution: "native"}
		if err := parsePipelineResources(map[string]any{field: float64(512)}, &req); err == nil {
			t.Fatal("native hard quota accepted")
		}
	}
	for _, tc := range []struct {
		cpu, mem int
		refuse   bool
	}{{1250, 1536, false}, {0, 0, false}, {2001, 1024, true}, {1000, 2049, true}} {
		req := pipelineStepRequest{execution: "container", cpuMilli: tc.cpu, memoryMiB: tc.mem}
		err := admitPipelineResourceLimits(&req, PipelinesPolicy{ContainerCPUMilli: 2000, ContainerMemoryMiB: 2048})
		if (err != nil) != tc.refuse {
			t.Fatalf("limits %+v: %v", tc, err)
		}
		if !tc.refuse && (req.cpuMilli == 0 || req.memoryMiB == 0) {
			t.Fatal("default resource ceiling missing")
		}
	}
}

func TestPipelineHeadroomIncludesEveryServiceAndBothPools(t *testing.T) {
	fakePipelineHostHeadroom(t)
	previous := pipelineDockerResources
	t.Cleanup(func() { pipelineDockerResources = previous })
	base := pipelineStepRequest{execution: "container", dockerID: "fixed", cpuMilli: 2000, memoryMiB: 2048, services: []pipelineService{{name: "one"}, {name: "two"}}}
	host := capacity.SystemHeadroom{Total: 16 << 30, Available: 10 << 30}
	docker := dockerResourceHeadroom{ID: "fixed", CPUs: 8, Total: 16 << 30, Used: 4 << 30}
	pipelineHostMemory = func() (capacity.SystemHeadroom, error) { return host, nil }
	pipelineHostCPUs = func() int { return 8 }
	pipelineDockerResources = func(context.Context) (dockerResourceHeadroom, error) { return docker, nil }
	if err := checkPipelineHeadroom(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"host memory", "host pressure", "host CPU", "Docker memory", "Docker CPU", "Docker identity", "Docker unavailable", "host unavailable"} {
		t.Run(kind, func(t *testing.T) {
			oldHost, oldDocker := host, docker
			oldMem, oldCPU, oldProbe := pipelineHostMemory, pipelineHostCPUs, pipelineDockerResources
			defer func() {
				host, docker = oldHost, oldDocker
				pipelineHostMemory, pipelineHostCPUs, pipelineDockerResources = oldMem, oldCPU, oldProbe
			}()
			switch kind {
			case "host memory":
				host.Available = (8 << 30) - 1
			case "host pressure":
				host.Pressure = true
			case "host CPU":
				pipelineHostCPUs = func() int { return 6 }
			case "Docker memory":
				docker.Used = (6 << 30) + 1
			case "Docker CPU":
				docker.CPUs = 6
			case "Docker identity":
				docker.ID = "replacement"
			case "Docker unavailable":
				pipelineDockerResources = func(context.Context) (dockerResourceHeadroom, error) {
					return dockerResourceHeadroom{}, errors.New("unavailable")
				}
			case "host unavailable":
				pipelineHostMemory = func() (capacity.SystemHeadroom, error) { return capacity.SystemHeadroom{}, errors.New("unavailable") }
			}
			if err := checkPipelineHeadroom(context.Background(), base); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

func TestPipelineResourceProbeIsBoundedAndMetadataOnly(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	fakePipelineDocker(t, `printf '%s\n' "$*" >> `+shellLiteral(log)+`
case "$1" in
info) printf '%s' '{"id":"fixed","cpus":8,"memory":17179869184}';;
stats) printf '1.5GiB / 2GiB\n512MiB / 2GiB\n';;
*) exit 9;;
esac`)
	got, err := readPipelineDockerResources(withPipelineDockerEndpoint(context.Background(), "unix:///tmp/memql-pipeline-test.sock"))
	if err != nil || got.ID != "fixed" || got.CPUs != 8 || got.Used != 2<<30 {
		t.Fatalf("probe %+v %v", got, err)
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "inspect") || !strings.Contains(string(calls), "stats --no-stream --format {{.MemUsage}}") {
		t.Fatalf("unexpected metadata scope %s", calls)
	}
	for _, raw := range []string{"unexpected", "NaNGiB / 2GiB", "-1MiB / 2GiB", "1e100GiB / 2GiB", "17GiB / 20GiB"} {
		if _, err := parsePipelineDockerUsage(raw, 16<<30); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	b := &pipelineProbeBuffer{limit: 4}
	n, err := b.Write([]byte("123456"))
	if n != 6 || err != nil || !b.exceeded || b.data.String() != "1234" {
		t.Fatal("output limit not enforced")
	}
}

func TestPipelineHeadroomRechecksAfterCheckoutBeforeCommand(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	policy, root := pipelineTestPolicy(t, "")
	count := 0
	pipelineHostMemory = func() (capacity.SystemHeadroom, error) {
		count++
		available := uint64(48 << 30)
		if count > 1 {
			available = 0
		}
		return capacity.SystemHeadroom{Total: 64 << 30, Available: available}, nil
	}
	marker := filepath.Join(t.TempDir(), "ran")
	_, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, "touch "+shellLiteral(marker), nil), policy, nil, nil)
	if fail == nil || fail.GetErrorCode() != "pipeline_capacity_busy" || count != 2 {
		t.Fatalf("admission %+v count=%d", fail, count)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran after headroom disappeared")
	}
	assertNoStepDirs(t, root)
	reservation, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.close()
	if reservation.dirty {
		t.Fatal("confirmed no-start leaked reservation")
	}
}

func TestPipelineContainerEnforcesRequestedLimits(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	policy, _ := pipelineTestPolicy(t, "")
	argv := filepath.Join(t.TempDir(), "argv")
	fakePipelineDocker(t, `case "$1" in
info) printf 'daemon-id\nlinux/arm64';;
create) printf '%s\n' "$@" > `+shellLiteral(argv)+`;;
image) echo linux/arm64;;
start) cat >/dev/null;;
inspect) printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}';;
rm|ps) exit 0;;
esac`)
	result := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx, "exit 0", map[string]any{"execution": "container", "platform": "linux/arm64", "image": containerTestDigest, "cpuMilli": 1250, "memoryMiB": 1536}), policy, nil, nil))
	if result.ExitCode != 0 {
		t.Fatal(result)
	}
	args, _ := os.ReadFile(argv)
	for _, want := range []string{"--cpus=1.250\n", "--memory=1536m\n", "--memory-swap=1536m\n"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestPipelineHeadroomReadsHostAfterDocker(t *testing.T) {
	fakePipelineHostHeadroom(t)
	previous := pipelineDockerResources
	t.Cleanup(func() { pipelineDockerResources = previous })
	order := ""
	pipelineDockerResources = func(context.Context) (dockerResourceHeadroom, error) {
		order += "docker;"
		return dockerResourceHeadroom{ID: "fixed", CPUs: 32, Total: 64 << 30}, nil
	}
	pipelineHostMemory = func() (capacity.SystemHeadroom, error) {
		order += "host;"
		return capacity.SystemHeadroom{Total: 64 << 30, Available: 48 << 30}, nil
	}
	if err := checkPipelineHeadroom(context.Background(), pipelineStepRequest{execution: "container", dockerID: "fixed", cpuMilli: 1000, memoryMiB: 2048}); err != nil {
		t.Fatal(err)
	}
	if order != "docker;host;" {
		t.Fatal(order)
	}
}
