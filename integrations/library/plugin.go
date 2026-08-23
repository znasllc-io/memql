package library

import (
	"context"

	"github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// init self-registers the library edit integration as a core plug-in.
// Always on: the document-version edit + restore capabilities back both
// the user-facing edit path (BFF) and the assistant editDocument tool
// (agent node), so they must be present on every node-type binary. The
// plug-in needs the engine handle to re-enter Execute for the read-then-
// append dance, so the factory plucks it off PluginContext.
//
// Anchored from app/plugins_core.go via a blank import so the init()
// runs at process start.
func init() {
	memql.RegisterPlugin("library", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		i := NewIntegration(pctx.Engine)
		i.SetLogger(pctx.Logger)
		// The analysis pass's text extractor (memql#4342). Built from the
		// SAME processor the attachment route uses, so "which types can
		// this node read" has one answer rather than two.
		//
		// The vision provider is resolved LAZILY, per call, which is why
		// PluginContext hands it over as a getter: factories run before
		// the provider registry is necessarily populated, and an image
		// uploaded an hour later must see the live provider rather than
		// whatever was there at boot.
		i.SetExtractor(lazyProcessor{vision: pctx.VisionProvider})
		return i, nil
	})
}

// lazyProcessor adapts component/fileprocessor to TextExtractor with a
// per-call vision-provider lookup. A nil getter (or one that returns nil)
// still yields a working processor for every non-image type; only image
// description needs the provider, and the processor's own image path is
// what reports its absence.
type lazyProcessor struct {
	vision func() common.VisionAIProvider
}

func (l lazyProcessor) Extract(ctx context.Context, mimeType string, data []byte) (string, error) {
	var provider common.VisionAIProvider
	if l.vision != nil {
		provider = l.vision()
	}
	return fileprocessor.NewDefaultProcessor(provider).Extract(ctx, mimeType, data)
}
