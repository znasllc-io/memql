//go:build agent

package app

import (
	"context"
	"os"
	"runtime"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/integrations/skills"
)

// integrations_skills.go -- `runScript` and `captureScript` on the agent node.
//
// ===========================================================================
// AGENT-ONLY, AND WIRED FROM THE TWO DISPATCHERS RATHER THAN OWNING ONE
// ===========================================================================
// A script runs as part of a step, and steps run on the agent node -- which
// is also the one node type that holds both a fleet dispatcher (its worker
// streams terminate here) and a route to the workbench. So this is where the
// two surfaces meet, and the composition needs no dispatcher of its own: it
// drives each one through the `dispatchHost` capability every other caller
// uses, so it cannot skip the environment hint, the safety classifier, the
// scope check, the router or the exec allowlist.
//
// ORDER MATTERS. This runs AFTER the workbench and worker integrations are
// registered, because it reads their capability handlers off the registry.
// A surface whose integration is absent is left nil, and the runner refuses
// by name (`no_script_surface`) rather than silently falling back to the
// other one -- falling back would run a step on the workbench that the
// caller's labels said needs a machine.
func (a *App) setupSkillsIntegration(fetcher skills.BlobFetcher, uploader server.FileUploader, bucket string) {
	if a == nil || a.engine == nil {
		return
	}

	workbench := dispatchHostHandler(a.engine, "workbench")
	fleet := dispatchHostHandler(a.engine, "agentworker")
	if workbench == nil && fleet == nil {
		// Neither surface on this binary. Registering an integration whose
		// every call refuses would put two builtins in the catalog that
		// cannot work; leaving them unregistered makes the executor audit say
		// so at boot instead.
		a.Logger.Info("skills integration: neither a workbench nor a fleet surface is wired; runScript is not registered")
		return
	}

	// The blob fetcher is the SAME Azure client the attachment and Library
	// routes use, handed over by the transport that built it. Absent -- a
	// cluster with no blob storage configured -- a script's bytes cannot be
	// read, and ReadArtifact says exactly that rather than shipping nothing
	// and reporting a hash mismatch.
	store := skills.NewEngineStore(engineExecutorForSkills{engine: a.engine}, fetcher)
	runner := skills.NewRunner(
		store,
		store,
		// The workbench runs THIS process's platform: it is a directory on
		// this node's own disk, so runtime.GOOS is the honest answer and not
		// a guess. The fleet's is deliberately unknown until the router picks.
		surfaceOrNil(workbench, func(h skills.CapabilityHandler) skills.Surface {
			return skills.NewWorkbenchSurface(h, runtime.GOOS)
		}),
		surfaceOrNil(fleet, skills.NewFleetSurface),
	)

	// The capture half. Absent -- a cluster with no object storage -- capture
	// refuses by name rather than filing nothing, which is what keeps a skill
	// from pointing at a machine path forever.
	if uploader != nil && strings.TrimSpace(bucket) != "" {
		runner = runner.WithLibrary(
			libraryArtifactWriter{store: server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: a.engine}), uploader: uploader, bucket: bucket},
			store,
		)
	}

	// The step journal, so a script step's dispatch is recorded on the row it
	// was made for. Nil-safe: a call naming no `stepId` records nothing
	// either way, which is every ad-hoc runScript.
	runner = runner.WithBindings(workjournal.New(
		workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			return a.engine.Execute(ctx, q)
		}),
		a.Logger,
		strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")),
	))

	if err := a.engine.RegisterIntegration(skills.NewIntegration(runner, a.Logger)); err != nil {
		a.fatal("skills integration: register failed", "error", err)
	}
	a.Logger.Info("skills integration registered",
		"workbench", workbench != nil, "fleet", fleet != nil,
		"capture", uploader != nil && strings.TrimSpace(bucket) != "")
}

// dispatchHostHandler plucks one integration's `dispatchHost` capability off
// the registry.
//
// BY NAME, off the PROVIDER interface, rather than by importing either
// package: `integrations/agent/worker` is agent-tagged and
// `integrations/workbench` is not, so importing both would tie this file's
// build constraints to theirs. The capability name is the contract, and a
// rename would show up here as a nil surface with a boot line naming it.
func dispatchHostHandler(engine *memql.MemQLEngine, integration string) skills.CapabilityHandler {
	provider := engine.IntegrationByName(integration)
	if provider == nil {
		return nil
	}
	for _, capability := range provider.Capabilities() {
		if capability.Name == "dispatchHost" && capability.Handler != nil {
			return skills.CapabilityHandler(capability.Handler)
		}
	}
	return nil
}

func surfaceOrNil(h skills.CapabilityHandler, build func(skills.CapabilityHandler) skills.Surface) skills.Surface {
	if h == nil {
		return nil
	}
	return build(h)
}

// engineExecutorForSkills adapts the engine to the store's narrow seam. The
// `any` result is discarded for a write and decoded for a read, which is what
// keeps integrations/skills off component/memql's import graph.
type engineExecutorForSkills struct{ engine *memql.MemQLEngine }

func (e engineExecutorForSkills) Execute(ctx context.Context, query string) (any, error) {
	return e.engine.Execute(ctx, query)
}

// ExecuteRows normalizes a read's result into row maps. A shape-projected
// query surfaces flat rows through OutputPayload; a raw bundle is walked as
// the fallback. Same shape as the seed materializer's own normalizer, and for
// the same reason -- which of the two a call produces depends on whether it
// carries a shape, and both spellings are legitimate.
func (e engineExecutorForSkills) ExecuteRows(ctx context.Context, query string) ([]map[string]any, error) {
	result, err := e.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	switch v := result.OutputPayload().(type) {
	case []map[string]any:
		return v, nil
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if row, ok := item.(map[string]any); ok {
				out = append(out, row)
			}
		}
		return out, nil
	case map[string]any:
		return []map[string]any{v}, nil
	}
	if result.Bundle == nil {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(result.Bundle.Nodes))
	for _, n := range result.Bundle.Nodes {
		if n == nil {
			continue
		}
		row := map[string]any{}
		if p := n.GetPayload(); p != nil {
			for k, val := range p.AsMap() {
				row[k] = val
			}
		}
		if id := n.GetId(); id != "" {
			if _, present := row["id"]; !present {
				row["id"] = id
			}
		}
		out = append(out, row)
	}
	return out, nil
}
