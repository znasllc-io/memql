//go:build !cognition && !planner

package fileprocessor

import (
	fp "github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the file-processing integration as a plug-in. Not
// compiled into cognition or planner binaries (which don't need text
// extraction -- they work off already-ingested content).
//
// The default processor uses the engine's VisionProvider for image
// description; when vision is unavailable the processor still handles
// text-based formats (PDF, DOCX, plain text) and only image extraction
// degrades.
func init() {
	memql.RegisterPlugin("files", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		processor := fp.NewDefaultProcessor(pctx.VisionProvider())
		return NewFilesIntegration(processor), nil
	})
}
