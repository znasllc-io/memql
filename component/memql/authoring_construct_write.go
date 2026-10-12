package memql

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// AuthoredConstructInput is one member construct a server pipeline writes into
// a bundle whose gates have not run yet.
type AuthoredConstructInput struct {
	ConstructId     string
	BundleId        string
	Kind            string
	Name            string
	TargetNamespace string
	Source          string
	// Origin and GrammarVersion are optional; empty leaves them unset.
	Origin         string
	GrammarVersion string
}

// WriteAuthoredConstruct writes one member construct into a bundle, as the
// actor ctx carries and under internal origin: createAuthoringConstruct is
// @serverOnly (memql#6022), because an active bundle arms every construct it
// holds.
//
// It is the one place a pipeline that may not stamp internal origin itself --
// the planner's capture and work compile -- hands that write to, the way the
// work spine's journal and the capability graph own their writers rather than
// their callers. What bounds it is the write guard
// (authoring_construct_write_guard.go): the construct joins only its own
// owner's bundle, and only before that bundle's gates have run, so nothing
// written here can reach a bundle that is armed.
func (e *MemQLEngine) WriteAuthoredConstruct(ctx context.Context, c AuthoredConstructInput) error {
	args := [][2]string{
		{"constructId", c.ConstructId},
		{"bundleId", c.BundleId},
		{"kind", c.Kind},
		{"name", c.Name},
		{"targetNamespace", c.TargetNamespace},
		{"source", c.Source},
	}
	if strings.TrimSpace(c.Origin) != "" {
		args = append(args, [2]string{"origin", c.Origin})
	}
	if strings.TrimSpace(c.GrammarVersion) != "" {
		args = append(args, [2]string{"grammarVersion", c.GrammarVersion})
	}
	_, err := e.Execute(auth.ContextWithInternalOrigin(ctx), mutationCall("createAuthoringConstruct", args...))
	return err
}
