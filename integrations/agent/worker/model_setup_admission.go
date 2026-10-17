package worker

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func modelStartPurpose(start *memqlv1.ModelCallStart) string {
	needs := NeedsForKind(start.GetKind())
	for _, m := range start.GetMessages() {
		needs.Vision = needs.Vision || len(m.GetImages()) > 0
	}
	return memqlengine.ModelPurpose(len(start.GetResponseFormatSchema()) > 0, len(start.GetTools()) > 0, needs.Vision, needs.Embeddings, needs.AudioIn, needs.AudioOut, needs.ImageGen)
}

// Re-read on the receiver after any wait: a BFF's cached choices and a sender's
// in-memory plan are not authority for the replica holding the worker stream.
func (h *ForwardHandler) admitModelSetup(ctx context.Context, w *workerservice.Worker, owner, registration string, start *memqlv1.ModelCallStart) (workerservice.SharingSubjects, func(), error) {
	row, subjects, err := h.admitSharedCandidate(ctx, owner, registration)
	if err != nil {
		return workerservice.SharingSubjects{}, nil, err
	}
	release := func() {}
	if row.ModelSetup.Revision != "" {
		release, err = w.AcquireModelSetup(ctx)
		if err != nil {
			return workerservice.SharingSubjects{}, nil, err
		}
		row, subjects, err = h.admitSharedCandidate(ctx, owner, registration)
		if err != nil {
			release()
			return workerservice.SharingSubjects{}, nil, err
		}
	}
	if w.OwnerUserId != "" && !sameSubject(w.OwnerUserId, owner) && !sameSubject(w.OwnerUserId, row.OwnerUserId) {
		release()
		return workerservice.SharingSubjects{}, nil, fmt.Errorf("machine ownership changed before the call")
	}
	if !row.ModelSetup.Allows(start.GetModel(), modelStartPurpose(start), 0) {
		release()
		return workerservice.SharingSubjects{}, nil, fmt.Errorf("this model is no longer selected for the requested task")
	}
	if row.ModelSetup.Revision != "" {
		if start.GetParams().GetContextTokens() > memqlengine.ModelSetupContextLimit {
			release()
			return workerservice.SharingSubjects{}, nil, fmt.Errorf("this setup supports up to 32K context")
		}
		if start.Params == nil {
			start.Params = &memqlv1.ModelCallParams{}
		}
		// An omitted runtime context must not become the model's much larger
		// advertised maximum, which the residency budget did not reserve.
		if start.Params.ContextTokens == 0 {
			start.Params.ContextTokens = memqlengine.ModelSetupContextLimit
		}
	}
	return subjects, release, nil
}
