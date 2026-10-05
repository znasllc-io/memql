package worker

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
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
	if req.ContextTokens <= 0 || (start.Kind != memqlengine.FleetKindChat && start.Kind != memqlengine.FleetKindVision) {
		return start, nil
	}
	attrs, ok := cand.ModelAttributesFor(req.ModelId)
	if !ok || attrs.ContextWindow < req.ContextTokens {
		return nil, fmt.Errorf("machine %s cannot allocate required context %d (advertised %d)", cand.Label(), req.ContextTokens, attrs.ContextWindow)
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
	allocated := proto.Clone(start).(*memqlv1.ModelCallStart)
	allocated.Params.ContextTokens = int64(window) // buildStart always supplies params
	return allocated, nil
}
