package worker

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// contextStartForCandidate separates the exact routing requirement from the
// runtime's allocation. Ollama reloads a runner when explicit num_ctx changes,
// even for two prompts differing by a few tokens. Stable allocation buckets
// avoid that reload without reserving the model's entire advertised ceiling.
//
// Do this AFTER placement: another machine advertising the same model may have
// a smaller capacity. The exact floor still decides eligibility, and neither a
// bucket nor its cap may reduce that floor. Copy the envelope so before-start
// fallback can allocate independently for its next candidate.
func contextStartForCandidate(req memqlengine.FleetCallRequest, cand Candidate, start *memqlv1.ModelCallStart) (*memqlv1.ModelCallStart, error) {
	if !allocatesContext(req, start.Kind) {
		return start, nil
	}
	window, err := allocatedContextWindow(req, cand)
	if err != nil {
		return nil, err
	}
	allocated := proto.Clone(start).(*memqlv1.ModelCallStart)
	allocated.Params.ContextTokens = int64(window) // buildStart always supplies params
	return allocated, nil
}

// contextTokensForCandidate is the window this call would ask this machine for,
// which is the figure the machine's memory gate charges for. Admission weighs the
// same one before any machine is tried. A machine that cannot allocate the window
// is refused before start whatever this says, so the call's own figure stands.
func contextTokensForCandidate(req memqlengine.FleetCallRequest, cand Candidate) int64 {
	kind := req.Kind
	if kind == "" {
		kind = workerservice.ModelCallKindChat
	}
	if allocatesContext(req, kind) {
		if window, err := allocatedContextWindow(req, cand); err == nil {
			return int64(window)
		}
	}
	return int64(req.ContextTokens)
}

func allocatesContext(req memqlengine.FleetCallRequest, kind string) bool {
	return req.ContextTokens > 0 && (kind == memqlengine.FleetKindChat || kind == memqlengine.FleetKindVision)
}

func allocatedContextWindow(req memqlengine.FleetCallRequest, cand Candidate) (int, error) {
	attrs, ok := cand.ModelAttributesFor(req.ModelId)
	if !ok || attrs.ContextWindow < req.ContextTokens {
		return 0, fmt.Errorf("machine %s cannot allocate required context %d (advertised %d)", cand.Label(), req.ContextTokens, attrs.ContextWindow)
	}
	window := 1
	for window < req.ContextTokens {
		// Avoid integer overflow, and stop at this machine's capacity when the
		// next power of two would exceed it. Capacity was proven >= required.
		if window > attrs.ContextWindow/2 {
			window = attrs.ContextWindow
			break
		}
		window *= 2
	}
	// No universal 32K floor: curated models budget different working windows,
	// and custom/small-machine models need only the next bucket above the call.
	return window, nil
}
