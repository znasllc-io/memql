package accounts

// The accounts plug-in: the two reads behind the Accounts app's inventory part
// (account row ownership, design record 2026-11-12, section H).
//
// Registered on EVERY node type, for the reason groups is: the builtins are
// reached from whichever node serves the OS shell's stream, and gating the
// registration by build tag would make a panel's answer depend on which
// replica a browser landed on.

import (
	"log/slog"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/env"
)

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
		return New(pctx.BunDB, memorynodes.List, activate != nil && !*activate, pctx.Logger), nil
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
