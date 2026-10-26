package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/imageverify"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func parsePipelineImageVerification(args map[string]any, req pipelineStepRequest) (pipelineStepRequest, error) {
	bad := func() (pipelineStepRequest, error) {
		return req, errors.New("pipeline_step: image verification requires the fixed isolated build contract")
	}
	if req.command != "" || argString(args, "execution") != "container" || argString(args, "platform") != "linux/amd64" || argString(args, "image") != "" {
		return bad()
	}
	for _, key := range []string{"services", "caches", "needs", "secrets", "githubReadToken", "fullHistory"} {
		if raw, ok := args[key]; ok && raw != nil {
			data, _ := json.Marshal(raw)
			if string(data) != "[]" && string(data) != "{}" && string(data) != "false" && string(data) != `""` {
				return bad()
			}
		}
	}
	raw, err := json.Marshal(args["imageBuild"])
	if err != nil || len(raw) > 128<<10 {
		return bad()
	}
	var build imageverify.Request
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&build) != nil {
		return bad()
	}
	if err = build.Validate(req.sha, "linux/amd64"); err != nil {
		return req, fmt.Errorf("pipeline_step: %w", err)
	}
	req.execution = "container"
	req.platform = "linux/amd64"
	req.imageBuild = &build
	if err = parsePipelineResources(args, &req); err != nil {
		return req, err
	}
	if req.artifacts, err = artifactList(args); err != nil {
		return req, err
	}
	if len(req.artifacts) != 1 || req.artifacts[0] != imageverify.Artifact {
		return bad()
	}
	// Only typed build arguments reach the builder. Pipeline bookkeeping env
	// is intentionally not inherited, nor are clone credentials retained after
	// the exact checkout finishes. The core compiler binds those arguments.
	return req, nil
}

var runImageVerification = imageverify.Run

func (r *pipelineRun) imageVerification(ctx context.Context) (int, *memqlv1.Failure) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, failure("bad_request", "image verification requires a deadline")
	}
	remaining := time.Until(deadline).Truncate(time.Second)
	if remaining < time.Second {
		return 0, r.stopped(ctx)
	}
	limits := imageverify.Limits{CPUMilli: r.req.cpuMilli, MemoryMiB: r.req.memoryMiB, MinFreeBytes: r.req.imageMinFreeBytes, Timeout: remaining}
	err := runImageVerification(ctx, r.dir, r.req.sha, *r.req.imageBuild, limits, func(record imageverify.Record) error {
		return r.reservation.begin(pipelineAttemptRecord{Execution: "image-verification", Workspace: r.dir, ImageVerification: &record})
	}, func(stderr bool, data []byte) {
		r.bytes.Add(int64(len(data)))
		r.emit(stderr, []byte(r.mask.mask(string(data))))
	})
	if errors.Is(err, imageverify.ErrCleanup) {
		return 0, failure("pipeline_cleanup_uncertain", "image verification runtime cleanup is unconfirmed; the machine remains reserved")
	}
	// Run either failed before runtime admission or confirmed its unit/cgroup
	// teardown. Only then may the existing workspace cleanup release the lease.
	if e := r.reservation.cleanupReady(r.dir); e != nil {
		return 0, failure("pipeline_recovery_required", e.Error())
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, r.stopped(ctx)
		}
		return 0, failure("pipeline_image_verification_failed", r.mask.mask(strings.TrimSpace(err.Error())))
	}
	return 0, nil
}
