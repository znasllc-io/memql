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

// AuthoringBundleWriter is the engine's one writer of a new draft bundle
// (memql#6022). createAuthoringBundle is @serverOnly -- a rule's next bundle id
// is known before it is written, and a bundle another person created there
// first would block its owner -- and this package does not stamp internal
// origin, so the capture and the work compile hand the write to the engine,
// which stamps it. The ids this package hands it are minted fresh. The App's
// planner engine implements it (app/adapters.go).
type AuthoringBundleWriter interface {
	WriteAuthoringBundle(ctx context.Context, b memql.AuthoringBundleInput) error
}

// writeAuthoringBundle writes b through engine's bundle writer. An engine
// without one cannot create a bundle at all.
func writeAuthoringBundle(ctx context.Context, engine any, b memql.AuthoringBundleInput) error {
	w, ok := engine.(AuthoringBundleWriter)
	if !ok {
		return fmt.Errorf("planner: this engine cannot create an authoring bundle (createAuthoringBundle is server-only, memql#6022)")
	}
	return w.WriteAuthoringBundle(ctx, b)
}

// BundleValidationRecorder is the engine's one recorder of Gate 1's verdict for
// this package (memql#6022). recordBundleValidation is @serverOnly -- a verdict
// is the claim that the gate ran, and a work template runs from a bundle only
// once it reads validated -- and this package does not stamp internal origin,
// so the capture and the work compile, each of which runs Gate 1 first, hand
// the write to the engine, which stamps it. The App's planner engine implements
// it (app/adapters.go).
type BundleValidationRecorder interface {
	RecordBundleValidation(ctx context.Context, v memql.BundleValidationInput) error
}

// recordBundleValidation records v through engine's recorder. An engine without
// one cannot record a verdict at all.
func recordBundleValidation(ctx context.Context, engine any, v memql.BundleValidationInput) error {
	r, ok := engine.(BundleValidationRecorder)
	if !ok {
		return fmt.Errorf("planner: this engine cannot record a Gate 1 verdict (recordBundleValidation is server-only, memql#6022)")
	}
	return r.RecordBundleValidation(ctx, v)
}
