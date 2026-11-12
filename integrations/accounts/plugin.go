package accounts

// The accounts plug-in: the two reads behind the Accounts app's inventory part
// (account row ownership, design record 2026-11-12, section H), and the
// account export's calls (account export, design record 2026-11-12, sections F
// and H).
//
// Registered on EVERY node type, for the reason groups is: the builtins are
// reached from whichever node serves the OS shell's stream, and gating the
// registration by build tag would make a panel's answer depend on which
// replica a browser landed on. The export RUNNER is the one agent-only part,
// started by app/accounts_export_agent.go over this same provider.

import (
	"context"
	"log/slog"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/email"
)

// IntegrationName is the plug-in's registered name, for the code that looks the
// provider up. The registration below spells the same word as a literal: the
// gate that classifies every plug-in (module_taxonomy_test.go) reads the name
// off the call and cannot follow a constant.
const IntegrationName = "accounts"

func init() {
	memql.RegisterPlugin("accounts", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// The same reading AccountOwnershipRunner takes (component/memql/
		// account_ownership_runner.go), which already refuses the boot on a
		// value that is not a boolean. Read here rather than asked of the
		// runner because the plug-in sees only PluginContext, and "held" is a
		// fact about this process's environment, captured once at start the
		// way the runner captures it.
		activate, err := env.NewEnvReader("MEMQL_ACCOUNT_STAMPING").OptionalBool("ACTIVATE")
		if err != nil {
			return nil, err
		}
		inv := New(pctx.BunDB, memorynodes.List, activate != nil && !*activate, pctx.Logger)
		return &Provider{Integration: inv, Exports: wireExports(inv, pctx), Moves: wireMoves(inv, pctx)}, nil
	})
}

// New builds the integration. db is resolved on every call because the
// database connects (and reconnects) after plug-ins are built; concepts is the
// concept registry's lister, a parameter so a test can name the concepts that
// declare an account form without loading the whole tree.
func New(db func() *bun.DB, concepts func() []*memorynodes.Concept, held bool, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{
		db: db, concepts: concepts, held: held, logger: logger,
		inventoryDeadline: inventoryDeadline, totalsDeadline: totalsDeadline,
	}
}

// Provider is what the plug-in registers: the inventory's Integration and the
// export half under the one name, so every `integration.accounts.<name>`
// executor reaches its handler. The download route and the agent's runner find
// it with engine.IntegrationByName(IntegrationName) and use its Exports.
type Provider struct {
	*Integration
	*Exports
	*Moves // the move calls (move_builtins.go)
}

// Capabilities implements memql.IntegrationProvider: both halves'.
func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return append(append(p.Integration.Capabilities(), p.Exports.exportCapabilities()...), p.Moves.moveCapabilities()...)
}

// engineWithIntegrations is the concrete engine's registry accessor, which the
// email sender is resolved through at send time.
type engineWithIntegrations interface {
	Integrations() *memql.IntegrationRegistry
}

// wireExports connects the export half to the engine. Every seam it cannot
// fill stays nil, and readiness reads a nil seam as a condition not met.
func wireExports(inv *Integration, pctx memql.PluginContext) *Exports {
	var engine Executor
	if pctx.Engine != nil {
		engine = pctx.Engine
	}
	x := NewExports(inv, engine, pctx.Logger)
	if e, ok := pctx.Engine.(*memql.MemQLEngine); ok && e != nil {
		x.assignmentStates = e.AccountAssignmentStates
	}
	x.followPending = memql.AccountFollowPending
	x.undeclared = memql.UndeclaredAccountConcepts
	x.blobs = lazyBlobs(func() (ExportBlobs, error) {
		u, err := azureblob.New(context.Background())
		if err != nil {
			return nil, err
		}
		return u.AccountExports(), nil
	})
	var registry engineWithIntegrations
	if r, ok := pctx.Engine.(engineWithIntegrations); ok {
		registry = r
	}
	x.notifyOwners = newOwnerNotice(activeOwnerEmails(engine), func() email.Sender {
		if registry == nil || registry.Integrations() == nil {
			return nil
		}
		emailInt, _ := registry.Integrations().Provider("email").(*email.Integration)
		if emailInt == nil {
			return nil
		}
		return emailInt.SenderAccess()
	}, x.logger)
	x.SetExportWork(newExportWork(x, pctx).Work())
	return x
}
