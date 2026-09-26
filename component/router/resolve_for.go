package router

// resolve_for.go -- the router's implementation of the engine's AIResolver
// seam (epic memql#5127, design D2).
//
// The engine cannot import this module (component/router imports
// component/memql and not the other way round), so the seam is an interface
// declared THERE and satisfied here, installed from app/ where both are
// visible. This file is that satisfaction and nothing else: it maps a modality
// onto the Resolve* entry point that serves it, and hands back the client with
// the decision that chose it.
//

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// ResolveFor satisfies memql.AIResolver.
func (r *Router) ResolveFor(ctx context.Context, req ResolveRequest) (memql.ResolvedProvider, error) {
	if r == nil {
		return memql.ResolvedProvider{}, memql.ErrAIResolverUnwired
	}
	// Some production callers (including AiChat) carry identity only in ctx.
	// Populate omitted attribution for selectors and observers; an explicit
	// server-side UserId retains the existing registry precedence.
	if strings.TrimSpace(req.UserId) == "" {
		if access, ok := auth.AccessFromContext(ctx); ok && access != nil {
			req.UserId = strings.TrimSpace(access.UserId)
		}
	}
	// AND WHAT KIND OF CALLER IT WAS (memql#5581), from the same context and
	// by the same belt-and-braces argument. The engine seam fills this for
	// every call that reaches a model through it; a request built OUTSIDE the
	// engine -- the agent replier builds its own -- arrives here without one,
	// and a blank caller kind on the decision record is exactly the ambiguity
	// the field exists to remove. CallerKindFromContext never answers "", so
	// an unattributed request is RECORDED as unattributed rather than blank.
	if strings.TrimSpace(req.CallerKind) == "" {
		req.CallerKind = auth.CallerKindFromContext(ctx)
	}

	var client any
	var resolved Resolved
	var err error

	switch req.Modality {
	case airoute.ModalityStreamingTools:
		client, resolved, err = r.resolveStreamWithTools(ctx, req)
	case airoute.ModalityTools:
		client, resolved, err = r.resolveWithTools(ctx, req)
	case airoute.ModalityStreamingChat:
		client, resolved, err = r.resolveStreamChat(ctx, req)
	case airoute.ModalityChat:
		client, resolved, err = r.resolveChat(ctx, req)
	case airoute.ModalityStructured:
		client, resolved, err = r.resolveDirect(ctx, req, modalityStructured)
	case airoute.ModalityVision:
		client, resolved, err = r.resolveDirect(ctx, req, modalityVision)
	case airoute.ModalityEmbedding:
		client, resolved, err = r.resolveDirect(ctx, req, modalityEmbedding)
	case airoute.ModalitySpeech:
		client, resolved, err = r.resolveAudio(ctx, req, modalitySpeech)
	case airoute.ModalityTranscribe:
		client, resolved, err = r.resolveAudio(ctx, req, modalityTranscribe)
	default:
		// A modality the seam does not serve is a CALL-SITE fault, and the
		// message says which: reporting it as an unavailable provider would
		// send somebody to look at their fleet for a call that never named a
		// door.
		return memql.ResolvedProvider{}, fmt.Errorf(
			"the router does not serve modality %q: chat, streamingChat, tools, streamingTools, "+
				"structured, vision, embedding, speech and transcribe are supported", req.Modality)
	}
	if err != nil {
		return memql.ResolvedProvider{}, err
	}
	// A SESSION WINNER'S CLIENT IS *NOT* SUBSTITUTED HERE, and the temptation
	// to is worth naming because it is wrong in a way nothing would report.
	//
	// resolved.Client holds the session client the chain walk built, and it
	// would be the obvious thing to hand back. But every client above is
	// already wrapped -- the tool and chat surfaces in a fallback wrapper that
	// wraps again in an OBSERVER, which is what writes the v1:router:call row.
	// Swapping the wrapper out for the bare session client would leave a run
	// that spent somebody's whole subscription with no ledger row at all, and
	// the call would work perfectly.
	//
	// The wrapper reaches the session on its own: providerLookup asks
	// sessionDoorFor the same question the walk did, which is exactly why that
	// decision lives in one function.
	return memql.ResolvedProvider{
		Client: client,
		Entry:  resolved.Entry,
		Resolution: airoute.Resolution{
			ProviderName: resolved.ProviderName,
			Vendor:       resolved.Vendor,
			Model:        resolved.Model,
			Decision:     resolved.Decision,
		},
	}, nil
}

// Compile-time proof that the seam is satisfied. Without it, a signature
// change on either side is discovered in app/ -- at the one call that installs
// the resolver, in a build-tagged file that not every lane compiles.
var _ memql.AIResolver = (*Router)(nil)

// Compile-time proof that the cache-hit half of the seam is satisfied too
// (memql#5581). It is a SEPARATE interface from AIResolver, so without this
// line a signature change would be discovered as a cache hit that silently
// stopped being recorded -- the exact failure shape the ledger exists to
// prevent.
var _ memql.AICacheRecorder = (*Router)(nil)
