package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type pipelineDockerEndpointKey struct{}

func validPipelineDockerEndpoint(endpoint string) bool {
	path, ok := strings.CutPrefix(endpoint, "unix://")
	return ok && len(endpoint) <= 2048 && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(endpoint, "\x00\r\n?#")
}

// Resolve the operator's selected local socket once, before measuring or
// recording work. Later context changes must not redirect this attempt.
func resolvePipelineDockerEndpoint(ctx context.Context) (string, error) {
	endpoint := os.Getenv("DOCKER_HOST")
	if os.Getenv("DOCKER_CONTEXT") != "" || endpoint == "" {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(probe, dockerBinary, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
		cmd.Env = stepEnvironment(os.Environ(), nil, nil)
		cmd.WaitDelay = 2 * time.Second
		out := &pipelineProbeBuffer{limit: 4096}
		errOut := &pipelineProbeBuffer{limit: 4096}
		cmd.Stdout, cmd.Stderr = out, errOut
		if err := cmd.Run(); err != nil || out.exceeded || errOut.exceeded || json.Unmarshal(out.data.Bytes(), &endpoint) != nil {
			return "", errors.New("pipeline_step: cannot resolve Docker's local socket")
		}
	}
	if !validPipelineDockerEndpoint(endpoint) {
		return "", errors.New("pipeline_step: Docker execution requires a local Unix socket")
	}
	return endpoint, nil
}

func withPipelineDockerEndpoint(ctx context.Context, endpoint string) context.Context {
	return context.WithValue(ctx, pipelineDockerEndpointKey{}, endpoint)
}

// Keep credentials and Docker configuration available, but neither ambient
// endpoint selector may override the socket recorded by this attempt.
func pipelineDockerEnvironment(machine []string, endpoint string) []string {
	env := stepEnvironment(machine, nil, nil)
	kept := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name != "DOCKER_CONTEXT" && name != "DOCKER_HOST" {
			kept = append(kept, entry)
		}
	}
	return append(kept, "DOCKER_HOST="+endpoint)
}

// A missing binding is an error, including during recovery. Never discover a
// new endpoint on behalf of a retained attempt that did not record one.
func dockerCommandAt(ctx context.Context, args ...string) (*exec.Cmd, error) {
	endpoint, _ := ctx.Value(pipelineDockerEndpointKey{}).(string)
	if !validPipelineDockerEndpoint(endpoint) {
		return nil, errors.New("pipeline_step: Docker endpoint is not bound")
	}
	cmd := exec.Command(dockerBinary, append([]string{"--host", endpoint}, args...)...)
	cmd.Env = pipelineDockerEnvironment(os.Environ(), endpoint)
	return cmd, nil
}

func verifyPipelineDockerDaemon(ctx context.Context, daemonID string) error {
	if daemonID == "" {
		return errPipelineUnreconciled
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := dockerOutput(probe, "info", "--format", "{{.ID}}")
	if err != nil || strings.TrimSpace(string(out)) != daemonID {
		return errors.New("interrupted container's Docker daemon is unavailable or changed; reconciliation required")
	}
	return nil
}
