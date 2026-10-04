//go:build workbench

package app

import (
	"os"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// integrationsWorkbench registers integration providers for a
// workbench node. The workbench binary is intentionally light --
// it loads:
//
//   - core integrations (database, auth, identity, embedding,
//     storage, etc.) via the plug-in path,
//   - the workbench plug-in itself (anchored in plugins_core.go),
//   - the pipeline runner (epic memql#5478), when this node can run
//     steps,
//
// and nothing else. No STT, no agent reply pipeline, no cognition
// integration -- those are agent / cognition / planner-node
// concerns. The workbench's only job is to receive
// WorkbenchForwardRequest messages on its NodeService.Stream and
// dispatch them to the local integration handler.
func (a *App) integrationsWorkbench() {
	a.integrationsCore()

	// THE PIPELINE RUNNER (#5493, #5495): a step's Kubernetes Job, its log
	// and its files in the owner's Library. Built here, installed on the
	// forward handler by the cluster phase (workbenchForwardHandler). A node
	// with no clone image or no in-cluster API server leaves it unset and
	// says why once; every pipeline forward to it then answers
	// pipelines_not_configured, having run nothing.
	a.pipelineRunner = a.workbenchPipelineRunner(
		pipelinesteps.ConfigFromEnv(os.Getenv),
		deploycontrol.InClusterAvailable(),
		deploycontrol.NewClusterAPI,
		a.resolveBlobStore,
	)

	a.Logger.Info("workbench integration providers registered")
}
