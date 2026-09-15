package automations

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/component"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// NewOfflineEngine builds the function registry used by loop analysis without
// connecting a database or starting event subscribers.
func NewOfflineEngine(logger *slog.Logger, registry concept.Registry) (*memql.MemQLEngine, error) {
	eng, err := memql.New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		return nil, err
	}
	if logger != nil {
		eng.Logger = logger
	}
	if err := eng.Init(registry); err != nil {
		return nil, err
	}
	return eng, nil
}

// LintLoopTree applies the strict automation check to a mounted product bundle
// and the embedded tree. It restores the global mount and concept state.
func LintLoopTree(logger *slog.Logger, root fs.FS) ([]memql.LintDiagnostic, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	_, _, unmount := memqldsl.MountOverlayDomains(logger, root)
	defer func() { unmount(); concept.ReplaceAll(nil); _, _ = memql.LoadUnifiedConcepts(logger) }()
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		return nil, err
	}
	eng, err := NewOfflineEngine(logger, concept.DefaultRegistry())
	if err != nil {
		return nil, fmt.Errorf("load loop analysis functions: %w", err)
	}
	loader := NewLoader(LoaderOptions{Logger: logger, Registry: concept.DefaultRegistry(), Functions: eng.Functions()})
	loaded, loadErr := loader.LoadAll()
	_, problems := loader.checkLoops(loaded)
	var diags []memql.LintDiagnostic
	for _, p := range problems {
		diags = append(diags, memql.LintDiagnostic{File: p.Path, Message: p.Err, Code: "loop_cycle"})
	}
	// A compile refusal must also fail lint even when no graph could be built.
	if loadErr != nil && len(problems) == 0 {
		return diags, loadErr
	}
	return diags, nil
}
