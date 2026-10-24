package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const maxPipelineServices = 4

var pipelineServiceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,58}$`)
var pipelineServiceReadyTimeout = 180 * time.Second
var pipelineServicePollInterval = 2 * time.Second

type pipelineService struct {
	name, image, ready string
	env                map[string]string
}

func parsePipelineServices(args map[string]any, req *pipelineStepRequest) error {
	raw, present := args["services"]
	if !present {
		return nil
	}
	services, ok := raw.(map[string]any)
	if !ok || len(services) > maxPipelineServices {
		return fmt.Errorf("pipeline_step: services must be an object with at most %d entries", maxPipelineServices)
	}
	if len(services) > 0 && req.execution != "container" {
		return errors.New("pipeline_step: services require container execution")
	}
	for _, name := range sortedKeys(services) {
		if !pipelineServiceName.MatchString(name) {
			return errors.New("pipeline_step: invalid service name")
		}
		fields, ok := services[name].(map[string]any)
		if !ok {
			return errors.New("pipeline_step: each service must be an object")
		}
		for key := range fields {
			if key != "image" && key != "env" && key != "ready" {
				return errors.New("pipeline_step: unknown service field")
			}
		}
		image, ok := fields["image"].(string)
		if !ok || !pipelineImageDigest.MatchString(image) {
			return errors.New("pipeline_step: each service requires an image pinned by sha256 digest")
		}
		ready := ""
		if value, present := fields["ready"]; present {
			ready, ok = value.(string)
			if !ok || strings.ContainsRune(ready, 0) || len(ready) > 8192 {
				return errors.New("pipeline_step: service ready must be a shell probe of at most 8192 bytes")
			}
		}
		env, err := stringMap(fields, "env")
		if err != nil {
			return err
		}
		req.services = append(req.services, pipelineService{name: name, image: image, ready: ready, env: env})
	}
	return nil
}

// Services share localhost, as they do in the cluster Job's pod. The first
// service owns that network namespace; all containers use an attempt-specific
// bridge. None publish host ports, mount the checkout/cache/socket, or receive
// the step's secrets. Their image entrypoints keep the default capabilities
// except NET_RAW so database bootstrap can switch to the image's own user.
// Names of EVERY resource are persisted before the first Docker mutation.
func (r *pipelineRun) startPipelineServices(ctx context.Context, record pipelineAttemptRecord) error {
	if len(r.req.services) == 0 {
		return nil
	}
	if err := r.serviceControl(ctx, "network", "create", "--driver=bridge", "--label", "io.memql.pipeline-attempt="+record.Container, record.Network); err != nil {
		return errors.New("pipeline_step: cannot create the services' network")
	}
	for i, svc := range r.req.services {
		network := record.Network
		if i > 0 {
			network = "container:" + record.Services[0]
		}
		args := []string{"create", "--name", record.Services[i], "--restart=no", "--init",
			"--label", "io.memql.pipeline-attempt=" + record.Container,
			"--platform", r.req.platform, "--pull=missing", "--network", network,
			"--cap-drop=NET_RAW", "--security-opt=no-new-privileges", "--pids-limit=512",
			"--cpus=2", "--memory=2g", "--memory-swap=2g"}
		for _, key := range sortedKeys(svc.env) {
			args = append(args, "--env", key+"="+svc.env[key])
		}
		if strings.TrimSpace(svc.ready) != "" {
			// Docker owns the probe process and kills it on timeout. Timing out
			// an attached `docker exec` client would leave its process running.
			args = append(args, "--health-cmd", svc.ready, "--health-interval=2s", "--health-timeout=5s", "--health-retries=90")
		} else {
			args = append(args, "--no-healthcheck")
		}
		args = append(args, svc.image)
		if err := r.serviceControl(ctx, args...); err != nil {
			return fmt.Errorf("pipeline_step: cannot create service %s", svc.name)
		}
		if err := checkPipelineImagePlatform(ctx, svc.image, r.req.platform); err != nil {
			return fmt.Errorf("pipeline_step: service %s image does not match the declared platform", svc.name)
		}
		if err := r.serviceControl(ctx, "start", record.Services[i]); err != nil {
			return fmt.Errorf("pipeline_step: cannot start service %s", svc.name)
		}
	}
	for i, svc := range r.req.services {
		if err := waitPipelineService(ctx, record.Services[i], strings.TrimSpace(svc.ready) != ""); err != nil {
			r.note(true, "memql: service "+svc.name+" failed readiness\n")
			// A bounded, streamed diagnostic; never aggregate repository output
			// in the worker's memory. Cancellation goes straight to cleanup.
			logs, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = r.serviceControl(logs, "logs", "--tail=20", record.Services[i])
			cancel()
			return fmt.Errorf("pipeline_step: service %s: %w", svc.name, err)
		}
	}
	return nil
}

func (r *pipelineRun) serviceControl(ctx context.Context, args ...string) error {
	result, err := runGroup(ctx, dockerCommand(args...), r.mask, r.emit)
	r.bytes.Add(result.bytes)
	if err != nil || result.exitCode != 0 || result.stopped {
		return errors.New("Docker service operation failed")
	}
	return nil
}

type pipelineServiceState struct {
	Running   bool
	OOMKilled bool
	Error     string
	Health    *struct{ Status string }
}

func readPipelineService(ctx context.Context, name string) (pipelineServiceState, error) {
	var state pipelineServiceState
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := dockerOutput(probe, "inspect", "--format", "{{json .State}}", name)
	if err != nil || json.Unmarshal(out, &state) != nil {
		return state, errors.New("cannot confirm service state")
	}
	if !state.Running || state.OOMKilled || state.Error != "" {
		return state, errors.New("service is not running")
	}
	return state, nil
}

func pipelineServiceRunning(ctx context.Context, name string) error {
	_, err := readPipelineService(ctx, name)
	return err
}

func waitPipelineService(ctx context.Context, name string, probe bool) error {
	ctx, cancel := context.WithTimeout(ctx, pipelineServiceReadyTimeout)
	defer cancel()
	for {
		state, err := readPipelineService(ctx, name)
		if err != nil {
			return err
		}
		if !probe || (state.Health != nil && state.Health.Status == "healthy") {
			return nil
		}
		if state.Health == nil || state.Health.Status == "unhealthy" {
			return errors.New("service readiness probe failed")
		}
		timer := time.NewTimer(pipelineServicePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("service readiness deadline exceeded")
		case <-timer.C:
		}
	}
}

func validPipelineAttempt(record pipelineAttemptRecord) bool {
	if record.Execution != "container" || record.DockerID == "" || !regexp.MustCompile(`^memql-step-[0-9a-f]{32}$`).MatchString(record.Container) || len(record.Services) > maxPipelineServices {
		return false
	}
	if len(record.Services) == 0 {
		return record.Network == ""
	}
	if record.Network != record.Container+"-net" {
		return false
	}
	for i, name := range record.Services {
		if name != fmt.Sprintf("%s-svc-%d", record.Container, i) {
			return false
		}
	}
	return true
}

func removePipelineAttempt(ctx context.Context, record pipelineAttemptRecord) error {
	if !validPipelineAttempt(record) {
		return errPipelineUnreconciled
	}
	var errs []error
	errs = append(errs, removePipelineContainer(ctx, record.Container))
	// Remove the command, then siblings, then the namespace owner, then bridge.
	for i := len(record.Services) - 1; i >= 0; i-- {
		errs = append(errs, removePipelineContainer(ctx, record.Services[i]))
	}
	if record.Network != "" {
		if _, err := dockerOutput(ctx, "network", "rm", record.Network); err != nil {
			out, listErr := dockerOutput(ctx, "network", "ls", "--filter", "name=^"+record.Network+"$", "--format", "{{.Name}}")
			if listErr != nil || strings.TrimSpace(string(out)) != "" {
				errs = append(errs, errors.New("service network removal unconfirmed"))
			}
		}
	}
	return errors.Join(errs...)
}
