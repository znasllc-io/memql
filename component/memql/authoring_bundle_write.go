package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// AuthoringBundleInput is a draft bundle a server pipeline creates before it
// writes the bundle's members.
type AuthoringBundleInput struct {
	BundleId string
	Title    string
	// Summary and SourceRunId are optional; empty leaves them unset.
	Summary     string
	SourceRunId string
	// ReusedConstructRefs is the list of composed constructs the bundle
	// records, any JSON-encodable list of {name, kind, namespace}; nil leaves
	// it unset.
	ReusedConstructRefs any
}

// WriteAuthoringBundle creates a draft bundle, as the actor ctx carries and
// under internal origin: createAuthoringBundle is @serverOnly (memql#6022),
// because a rule's next bundle id is known before it is written, and a bundle
// another person created there first would block its owner.
//
// It is the one place a pipeline that may not stamp internal origin itself --
// the planner's capture and work compile -- hands that write to, as
// WriteAuthoredConstruct is for the bundle's members, and the durable promote
// writes its bundle through it too. Every caller mints the bundle's id fresh;
// the bundle is stamped the caller's, and its members are held to it by the
// construct write guard.
func (e *MemQLEngine) WriteAuthoringBundle(ctx context.Context, b AuthoringBundleInput) error {
	args := map[string]any{
		"bundleId": b.BundleId,
		"title":    b.Title,
	}
	if strings.TrimSpace(b.Summary) != "" {
		args["summary"] = b.Summary
	}
	if strings.TrimSpace(b.SourceRunId) != "" {
		args["sourceRunId"] = b.SourceRunId
	}
	if b.ReusedConstructRefs != nil {
		args["reusedConstructRefs"] = b.ReusedConstructRefs
	}
	call, err := languageParser.RenderCall("createAuthoringBundle", args)
	if err != nil {
		return fmt.Errorf("authoring: render the bundle write: %w", err)
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}

// BundleValidationInput is Gate 1's verdict on a bundle, as a server pipeline
// that ran the gate records it.
type BundleValidationInput struct {
	BundleId string
	// Status is "validated" or "failed".
	Status string
	// Report is the structured verdict (validationReport): any JSON-encodable
	// object.
	Report any
	// FailureReason is optional; empty leaves it unset.
	FailureReason string
}

// RecordBundleValidation records Gate 1's verdict on a bundle, as the actor ctx
// carries and under internal origin: recordBundleValidation is @serverOnly
// (memql#6022), because a verdict is the claim that the gate ran -- a work
// template runs from a bundle only once it reads validated.
//
// It is the one place a pipeline that may not stamp internal origin itself --
// the planner's capture and work compile, each of which runs Gate 1 before it
// records the verdict -- hands that write to, as WriteAuthoringBundle is for
// the bundle itself.
func (e *MemQLEngine) RecordBundleValidation(ctx context.Context, v BundleValidationInput) error {
	args := map[string]any{
		"bundleId":         v.BundleId,
		"status":           v.Status,
		"validationReport": v.Report,
	}
	if strings.TrimSpace(v.FailureReason) != "" {
		args["failureReason"] = v.FailureReason
	}
	call, err := languageParser.RenderCall("recordBundleValidation", args)
	if err != nil {
		return fmt.Errorf("authoring: render the Gate 1 verdict: %w", err)
	}
	_, err = e.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}
