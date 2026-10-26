package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/core/num"
)

const pipelineServiceCPUMilli = 2000
const pipelineServiceMemoryMiB = 2048
const pipelineMemoryReserve = uint64(2 << 30)

var pipelineHostMemory = capacity.ReadSystemHeadroom
var pipelineHostCPUs = runtime.NumCPU
var pipelineDockerResources = readPipelineDockerResources

// The wire uses JSON numbers. Validate before narrowing and reject fractional,
// null and string values instead of silently replacing them with defaults.
func pipelineResourceNumber(args map[string]any, name string, low, high int) (int, error) {
	raw, present := args[name]
	if !present {
		return 0, nil
	}
	var value float64
	switch n := raw.(type) {
	case float64:
		value = n
	case int:
		value = float64(n)
	default:
		return 0, fmt.Errorf("pipeline_step: %s must be a whole JSON number", name)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value < 0 || value > float64(high) || (value != 0 && value < float64(low)) {
		return 0, fmt.Errorf("pipeline_step: %s is outside its supported range", name)
	}
	return num.Float64OrZero(value), nil
}

func parsePipelineResources(args map[string]any, req *pipelineStepRequest) error {
	var err error
	req.cpuMilli, err = pipelineResourceNumber(args, "cpuMilli", 1, 256000)
	if err != nil {
		return err
	}
	req.memoryMiB, err = pipelineResourceNumber(args, "memoryMiB", 128, 1048576)
	if err != nil {
		return err
	}
	if req.execution == "native" && (req.cpuMilli != 0 || req.memoryMiB != 0) {
		return errors.New("pipeline_step: hard resource limits require container execution")
	}
	return nil
}

func admitPipelineResourceLimits(req *pipelineStepRequest, policy PipelinesPolicy) error {
	cpu, memory := policy.ContainerCPUMilli, policy.ContainerMemoryMiB
	if cpu == 0 {
		cpu = DefaultPipelineContainerCPUMilli
	}
	if memory == 0 {
		memory = DefaultPipelineContainerMemoryMiB
	}
	if req.cpuMilli > cpu || req.memoryMiB > memory {
		return errors.New("pipeline_step: requested command resources exceed this machine's pipelines policy")
	}
	if req.cpuMilli == 0 {
		req.cpuMilli = cpu
	}
	if req.memoryMiB == 0 {
		req.memoryMiB = memory
	}
	return nil
}

type dockerResourceHeadroom struct {
	ID          string
	CPUs        int
	Total, Used uint64
}

// Fixed metadata only. Never collect container commands, names, environment or
// inspect payloads. Bound stdout/stderr and the lifetime of each Docker probe.
func pipelineResourceOutput(ctx context.Context, args ...string) ([]byte, error) {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bound, err := dockerCommandAt(ctx, args...)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(probe, bound.Path, bound.Args[1:]...)
	cmd.Env = bound.Env
	cmd.WaitDelay = 2 * time.Second
	out := &pipelineProbeBuffer{limit: 65536}
	cmd.Stdout = out
	errOut := &pipelineProbeBuffer{limit: 4096}
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		return nil, errors.New("Docker resource measurement unavailable")
	}
	if out.exceeded || errOut.exceeded {
		return nil, errors.New("Docker resource measurement exceeded its bound")
	}
	return out.data.Bytes(), nil
}

type pipelineProbeBuffer struct {
	data     bytes.Buffer
	limit    int
	exceeded bool
}

func (b *pipelineProbeBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > b.limit-b.data.Len() {
		b.exceeded = true
		p = p[:max(0, b.limit-b.data.Len())]
	}
	_, _ = b.data.Write(p)
	return n, nil
}

func readPipelineDockerResources(ctx context.Context) (dockerResourceHeadroom, error) {
	raw, err := pipelineResourceOutput(ctx, "info", "--format", `{"id":{{json .ID}},"cpus":{{.NCPU}},"memory":{{.MemTotal}}}`)
	if err != nil {
		return dockerResourceHeadroom{}, err
	}
	var info struct {
		ID     string `json:"id"`
		CPUs   int    `json:"cpus"`
		Memory uint64 `json:"memory"`
	}
	if err = json.Unmarshal(raw, &info); err != nil || info.ID == "" || len(info.ID) > 256 || info.CPUs < 1 || info.CPUs > 4096 || info.Memory == 0 {
		return dockerResourceHeadroom{}, errors.New("incomplete Docker resource measurement")
	}
	raw, err = pipelineResourceOutput(ctx, "stats", "--no-stream", "--format", "{{.MemUsage}}")
	if err != nil {
		return dockerResourceHeadroom{}, err
	}
	used, err := parsePipelineDockerUsage(string(raw), info.Memory)
	if err != nil {
		return dockerResourceHeadroom{}, err
	}
	return dockerResourceHeadroom{ID: info.ID, CPUs: info.CPUs, Total: info.Memory, Used: used}, nil
}

func parsePipelineDockerUsage(raw string, total uint64) (uint64, error) {
	var used uint64
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		usage, limit, ok := strings.Cut(line, " / ")
		if !ok {
			return 0, errors.New("invalid Docker memory measurement")
		}
		value, err := pipelineByteQuantity(usage)
		if err != nil {
			return 0, err
		}
		if _, err = pipelineByteQuantity(limit); err != nil {
			return 0, err
		}
		if value > total || used > total-value {
			return 0, errors.New("Docker memory measurement exceeds its pool")
		}
		used += value
	}
	return used, nil
}

func pipelineByteQuantity(raw string) (uint64, error) {
	units := []struct {
		name   string
		factor float64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	for _, unit := range units {
		if s, ok := strings.CutSuffix(strings.TrimSpace(raw), unit.name); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			value := n * unit.factor
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(1<<60) {
				break
			}
			return uint64(math.Ceil(value)), nil
		}
	}
	return 0, errors.New("invalid Docker memory quantity")
}

func checkPipelineHeadroom(ctx context.Context, req pipelineStepRequest) error {
	cpu := req.cpuMilli + len(req.services)*pipelineServiceCPUMilli
	memory := uint64(req.memoryMiB+len(req.services)*pipelineServiceMemoryMiB) << 20
	var docker dockerResourceHeadroom
	usesDocker := req.execution == "container" || slices.Contains(req.needs, "docker")
	if usesDocker {
		var err error
		docker, err = pipelineDockerResources(ctx)
		if err != nil || docker.ID != req.dockerID || docker.Total == 0 || docker.Used > docker.Total {
			return errors.New("pipeline_step: current Docker resource evidence unavailable; no pipeline command started")
		}
	}
	// Host evidence is measured last: slow Docker metadata cannot age the host
	// snapshot by its probe timeout before a command is admitted.
	host, err := pipelineHostMemory()
	if err != nil || host.Total == 0 || host.Available > host.Total || host.Pressure {
		return errors.New("pipeline_step: healthy host RAM evidence unavailable; no pipeline command started")
	}
	reserve := max(pipelineMemoryReserve, host.Total/8)
	// Keep at least one host CPU and RAM headroom outside the entire step,
	// including every service. The shared host lease serializes our own clients;
	// this does not constrain unrelated applications or native commands.
	if cpu > (pipelineHostCPUs()-1)*1000 || host.Available < reserve || memory > host.Available-reserve {
		return errors.New("pipeline_step: insufficient host CPU or RAM headroom for command and services; no pipeline command started")
	}
	if !usesDocker {
		return nil
	}
	// Docker reports container usage rather than VM MemAvailable. Retain a
	// separate quarter-pool reserve for its kernel, caches and untracked work.
	reserve = max(pipelineMemoryReserve, docker.Total/4)
	available := docker.Total - docker.Used
	if cpu > (docker.CPUs-1)*1000 || available < reserve || memory > available-reserve {
		return errors.New("pipeline_step: insufficient Docker CPU or RAM headroom for command and services; no pipeline command started")
	}

	return nil
}
