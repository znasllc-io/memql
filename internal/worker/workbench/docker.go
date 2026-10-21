package workbench

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

//go:embed program.py
var program string

type Docker struct{ Binary, endpoint string }

func (d Docker) binary() string {
	if d.Binary != "" {
		return d.Binary
	}
	return "docker"
}

type bounded struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *bounded) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining < 0 {
		remaining = 0
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.exceeded = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func (d Docker) command(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	if d.endpoint != "" {
		args = append([]string{"--host", d.endpoint}, args...)
	}
	cmd := exec.CommandContext(ctx, d.binary(), args...)
	if d.endpoint != "" {
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "DOCKER_CONTEXT=") && !strings.HasPrefix(entry, "DOCKER_HOST=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
	}
	// No caller environment, working directory or credentials enter this process.
	cmd.Stdin = input
	cmd.WaitDelay = 2 * time.Second
	out := &bounded{limit: 3 * MaxOutput}
	errOut := &bounded{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		// Docker's own words are the diagnosis ("Cannot connect to the
		// Docker daemon ...", "no space left on device"); dropping them
		// left an exit status naming nothing (memql-cockpit#524).
		if why := firstLine(errOut.String()); why != "" {
			return nil, fmt.Errorf("Docker %s failed: %w: %s", args[0], err, why)
		}
		return nil, fmt.Errorf("Docker %s failed: %w", args[0], err)
	}
	if out.exceeded {
		return nil, fmt.Errorf("Docker response exceeded its limit")
	}
	return out.Bytes(), nil
}

// Readiness requires room for one workspace. Admission accounts for existing
// workloads before each creation; the maximum slot count is not an allocation.
const (
	minCPUs        = 3
	minMemoryBytes = int64(MemoryMiB) * 1024 * 1024 * 5 / 4
)

// firstLine keeps one line of a command's stderr for an error message.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

type Runtime struct {
	ID          string
	Endpoint    string
	CPUs        int
	MemoryBytes int64
}

func (d Docker) Ready(ctx context.Context) (Runtime, error) {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint, err := d.localEndpoint(probe)
	if err != nil {
		return Runtime{}, err
	}
	d.endpoint = endpoint
	out, err := d.command(probe, nil, "info", "--format", "{{json .}}")
	if err != nil {
		return Runtime{}, err
	}
	var info struct {
		ID, OSType string
		NCPU       int
		MemTotal   int64
	}
	if json.Unmarshal(out, &info) != nil || info.ID == "" || info.OSType != "linux" {
		return Runtime{}, fmt.Errorf("fleet workbenches require Docker running Linux containers")
	}
	// Use reported memory, allowing the kernel's share of the configured VM.
	// No emulation or daemon reconfiguration is done.
	if info.NCPU < minCPUs || info.MemTotal < minMemoryBytes {
		return Runtime{}, fmt.Errorf("Docker needs at least 3 CPUs and 2 GB for a workbench with headroom; it has %d CPUs and %.1f GB", info.NCPU, float64(info.MemTotal)/1e9)
	}
	if _, err = d.command(probe, nil, "image", "inspect", Image); err != nil {
		return Runtime{}, fmt.Errorf("workbench runtime is not installed; run Cockpit workbench setup")
	}
	return Runtime{ID: info.ID, Endpoint: endpoint, CPUs: info.NCPU, MemoryBytes: info.MemTotal}, nil
}
func (d Docker) Pull(ctx context.Context) error {
	endpoint, err := d.localEndpoint(ctx)
	if err != nil {
		return err
	}
	d.endpoint = endpoint
	_, err = d.command(ctx, nil, "pull", Image)
	return err
}
func containerArgs(name, key string) []string {
	return []string{"run", "--detach", "--name", name, "--label", "io.memql.fleet-workbench=" + key, "--restart=no", "--init", "--read-only", "--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=128", "--cpus=1", "--memory=" + strconv.Itoa(MemoryMiB) + "m", "--memory-swap=" + strconv.Itoa(MemoryMiB) + "m", "--user=65532:65532", "--tmpfs", "/workspace:rw,nosuid,nodev,size=" + strconv.Itoa(WorkspaceMiB) + "m,uid=65532,gid=65532,mode=0700", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=64m,uid=65532,gid=65532,mode=0700", "--workdir=/workspace", "--env=HOME=/tmp", "--log-driver=none", "--pull=never", "--entrypoint=python3", Image, "-c", "import time; time.sleep(3600)"}
}
func (d Docker) Create(ctx context.Context, runtime Runtime, name, key string) error {
	d.endpoint = runtime.Endpoint
	if err := d.verifyDaemon(ctx, runtime); err != nil {
		return err
	}
	_, err := d.command(ctx, nil, containerArgs(name, key)...)
	return err
}
func (d Docker) Remove(ctx context.Context, runtime Runtime, name string) error {
	d.endpoint = runtime.Endpoint
	if err := d.verifyDaemon(ctx, runtime); err != nil {
		return err
	}
	_, err := d.command(ctx, nil, "rm", "--force", name)
	if err == nil {
		return nil
	}
	// rm is idempotent only after the daemon proves the name absent.
	out, listErr := d.command(ctx, nil, "ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if listErr == nil && strings.TrimSpace(string(out)) == "" {
		return nil
	}
	return err
}
func (d Docker) Execute(ctx context.Context, runtime Runtime, name string, request Request) (Result, error) {
	d.endpoint = runtime.Endpoint
	if err := d.verifyDaemon(ctx, runtime); err != nil {
		return Result{}, err
	}
	input, err := json.Marshal(request)
	if err != nil || len(input) > MaxInput {
		return Result{}, fmt.Errorf("workbench input is too large")
	}
	out, err := d.command(ctx, bytes.NewReader(input), "exec", "--interactive", name, "python3", "-c", program)
	if err != nil {
		return Result{}, err
	}
	var result Result
	if err = json.Unmarshal(out, &result); err != nil || result.Action != request.Action {
		return Result{}, fmt.Errorf("workbench did not return a valid operation result")
	}
	return result, nil
}

// Bind operations to the local socket selected at admission. Changing Docker's
// current context cannot redirect an existing workspace or its cleanup.
func (d Docker) localEndpoint(ctx context.Context) (string, error) {
	if _, err := exec.LookPath(d.binary()); err != nil {
		return "", fmt.Errorf("Docker is not installed; install Docker Desktop (or Docker Engine) and retry")
	}
	endpoint := os.Getenv("DOCKER_HOST")
	if os.Getenv("DOCKER_CONTEXT") != "" || endpoint == "" {
		out, err := d.command(ctx, nil, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
		if err != nil || json.Unmarshal(out, &endpoint) != nil {
			return "", fmt.Errorf("cannot resolve Docker's local socket")
		}
	}
	if !strings.HasPrefix(endpoint, "unix:///") || strings.ContainsAny(endpoint, "\x00\r\n") {
		return "", fmt.Errorf("fleet workbenches require a local Docker Unix socket")
	}
	return endpoint, nil
}
func (d Docker) verifyDaemon(ctx context.Context, runtime Runtime) error {
	if runtime.ID == "" || !strings.HasPrefix(runtime.Endpoint, "unix:///") {
		return ErrInterrupted
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := d.command(probe, nil, "info", "--format", "{{.ID}}")
	if err != nil || strings.TrimSpace(string(out)) != runtime.ID {
		return ErrInterrupted
	}
	return nil
}
