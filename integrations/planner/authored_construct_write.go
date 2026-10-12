package planner

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// AuthoredConstructWriter is the engine's one writer of a bundle's member
// constructs (memql#6022). createAuthoringConstruct is @serverOnly -- an
// active bundle arms every construct it holds -- and this package does not
// stamp internal origin, so the capture and the work compile hand the write to
// the engine, which stamps it, and whose write guard admits it only into the
// owner's own bundle before that bundle's gates have run. The App's planner
// engine implements it (app/adapters.go).
type AuthoredConstructWriter interface {
	WriteAuthoredConstruct(ctx context.Context, c memql.AuthoredConstructInput) error
}

// writeAuthoredConstruct writes c through engine's construct writer. An engine
// without one cannot write a member construct at all.
func writeAuthoredConstruct(ctx context.Context, engine any, c memql.AuthoredConstructInput) error {
	w, ok := engine.(AuthoredConstructWriter)
	if !ok {
		return fmt.Errorf("planner: this engine cannot write an authored construct (createAuthoringConstruct is server-only, memql#6022)")
	}
	return w.WriteAuthoredConstruct(ctx, c)
}
