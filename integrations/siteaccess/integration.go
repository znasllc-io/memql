package siteaccess

import (
	"context"
	"fmt"
	"log/slog"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// integration.go -- the six DSL-callable capabilities:
//
//	integration.siteAccess.create      mint a code             (dsl builtin siteAccessCodeCreate)
//	integration.siteAccess.regenerate  replace its secret      (dsl builtin siteAccessCodeRegenerate)
//	integration.siteAccess.setExpiry   move its expiry         (dsl builtin siteAccessCodeSetExpiry)
//	integration.siteAccess.remove      end it                  (dsl builtin siteAccessCodeRemove)
//	integration.siteAccess.gates       the gates a site opens  (dsl builtin siteAccessGates)
//	integration.siteAccess.list        the codes of a site     (dsl builtin siteAccessCodesForSite)
//
// Every client-callable read and write on access codes is one of these, passing the shared
// organization gate; the queries and mutations beneath them are @serverOnly. The list is a builtin,
// and reads its rows through the @serverOnly siteAccessCodeRowsForSite with internal origin and the
// caller's actor untouched, because that gate judges builtins and mutations and never a query. The
// rows are readable by no client any other way (clientUnreadableConcepts, component/memql), so these
// handlers and the bff are the readers.
//
// # Why every handler refuses for now
//
// The DSL contract lands before the behaviour behind it, so that everything
// downstream -- the generated SDK, the OS's reads, the bff's store -- is
// written against constructs that exist. The builtins have to RESOLVE at boot,
// which is what a registered capability is; they do not yet have to WORK. Until
// the handlers are filled in, each one refuses by name rather than answering,
// because a stub that returned an empty reply would read as a success: a code
// that was never minted, a gate list that is merely empty.

// Integration exposes the access code capabilities.
type Integration struct {
	logger *slog.Logger
}

// NewIntegration builds the integration.
func NewIntegration(logger *slog.Logger) *Integration {
	return &Integration{logger: logger}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "siteAccess" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "create",
			Description: "Mint an access code that opens one gate of a deployable. " +
				"Returns the plain code once; only its SHA-256 is stored.",
			Handler: notImplemented("siteAccessCodeCreate"),
			ArgsSchema: map[string]string{
				"siteId":    "string (required) -- the deployable the code opens a part of.",
				"label":     "string (required) -- the operator's name for the code, 1 to 80 characters.",
				"gate":      "string (required) -- the gate the code opens.",
				"subject":   "string (optional) -- scope within the gate, at most 120 characters.",
				"expiresAt": "datetime (required) -- when the code stops opening the site.",
			},
		},
		{
			Name: "regenerate",
			Description: "Replace an access code's secret, keeping its id, label, gate and subject. " +
				"Ends every session opened with the old secret. Returns the plain code once.",
			Handler: notImplemented("siteAccessCodeRegenerate"),
			ArgsSchema: map[string]string{
				"codeId":    "string (required) -- the access code to regenerate.",
				"expiresAt": "datetime (optional) -- a new expiry; omitted keeps the current one.",
			},
		},
		{
			Name: "setExpiry",
			Description: "Change when an access code stops opening the site, without changing the code. " +
				"Extending an expired code revives it.",
			Handler: notImplemented("siteAccessCodeSetExpiry"),
			ArgsSchema: map[string]string{
				"codeId":    "string (required) -- the access code to re-time.",
				"expiresAt": "datetime (required) -- the new expiry.",
			},
		},
		{
			Name:        "remove",
			Description: "End an access code. A soft end: the row stays as the record that the code existed.",
			Handler:     notImplemented("siteAccessCodeRemove"),
			ArgsSchema: map[string]string{
				"codeId": "string (required) -- the access code to end.",
			},
		},
		{
			Name: "gates",
			Description: "The gates a deployable declares with @accessGate, and whether each reads the " +
				"accessSubject argument. One row per gate.",
			Handler: notImplemented("siteAccessGates"),
			ArgsSchema: map[string]string{
				"siteId": "string (required) -- the deployable to list the gates of.",
			},
		},
		{
			Name: "list",
			Description: "The access codes minted against a deployable, removed and expired ones included, one row per " +
				"code and never the code or its digest, each with lastOpenedAt taken from the newest session of its " +
				"current generation when there is one. No order is promised.",
			Handler: notImplemented("siteAccessCodesForSite"),
			ArgsSchema: map[string]string{
				"siteId": "string (required) -- the deployable to list the codes of.",
			},
		},
	}
}

// notImplemented is the handler every capability carries until its behaviour
// lands. It names the construct so an operator reading the refusal knows which
// act did not run, and it writes nothing.
func notImplemented(construct string) func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
		return nil, fmt.Errorf("%s: not implemented yet -- the access codes feature is declared in the platform DSL and has no behaviour behind it on this build", construct)
	}
}
