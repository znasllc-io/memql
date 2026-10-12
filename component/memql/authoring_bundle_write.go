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
