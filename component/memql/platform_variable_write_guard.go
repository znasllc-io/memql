package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A CONFIGURATION VARIABLE'S ROW IS WRITTEN BY THE DEPLOYMENT'S AUDIENCE.
//
// v1:platform:globalVariable and v1:platform:partitionVariable hold the
// deployment's plaintext configuration. The resolvers read a row by NAME
// (ResolveVariable / ResolveSystemVariable, engine_variables.go), and the
// callers that fall back to the environment consult the row first. So whoever
// may write a row decides what a name means on every node: which federation ids
// a provider loads, which GitHub App this cluster is, where its mail leaves
// from, whether a pipeline asks a model.
//
// THE RULE. A write to either concept is admitted for the audience of the
// row-authz write escape (rowauthz_write_guard.go) and refused for everyone
// else:
//
//   - a cluster owner. The operator credential's stream is one, which is how
//     scripts/secrets seeds these rows over the wire, and so are the
//     deployment's own system actors: the boot injector, an automation of the
//     registered tree.
//   - trusted server-side Go stamping internal origin for this one write, after
//     it has authorized its caller. That is the way in for a person who is not
//     a cluster owner: a settings save (integrationConfigure,
//     providerFederationSet, the Pipelines settings saves) derives the row's
//     name itself, validates the value and then writes.
//
// It covers the named mutations, the raw insert literal and an inline or
// authored body alike, on a fresh id or over a stored row, because
// executeWrite is where they converge. A gate declared on setGlobalVariable
// would be judged for that one call and for none of the others.
//
// WHY NOT A ROW TIER. @rowAuthz(clusterOwner) states exactly this write rule
// and is how the two SECRET concepts state it. But a tier decides reads too,
// and these rows are read by design: MemQL OS draws its settings from them, DSL
// logic reads its windows and graces through `query globalVariable(...)`, and
// Go readers hold whatever context they were handed, some with no identity at
// all. `rankFloor` relaxes the tier's read only down to a rung of the role
// ladder, and a caller with no identity and an account-scoped role both stand
// below every rung -- each would read ZERO ROWS, which is the answer for a
// setting nobody made, so it would read as configuration and never as this. The
// secret concepts could take the tier because one reader, the resolver, wants
// their rows. TestVariableRowsAreStillReadByEveryCaller pins the read side.
//
// WHY NOT @serverWritten. It admits internal origin alone, and a cluster owner
// writes these rows over the wire: the operator CLI, and an owner from MemQL
// OS, a script or an editor.
//
// WHAT THIS DOES NOT DECIDE. Which names a settings save may write is that
// handler's to say, from its own manifest. And a body the registered or an
// authored tree runs under a system actor writes as the deployment: authoring
// DSL is already authority over the whole cluster.
//
// Regression tests: platform_variable_write_db_test.go (every role, both
// concepts, named and raw, fresh and stored; the writers that must still land;
// reads unchanged) and component/grpc's variable_write_wire_db_test.go (the
// same through the request handler).

// platformVariableConcepts are the concepts this rule governs: the two the
// variable resolvers read.
var platformVariableConcepts = map[string]bool{
	memorynodes.ConceptPlatformGlobalVariable:    true,
	memorynodes.ConceptPlatformPartitionVariable: true,
}

// guardPlatformVariableWrite refuses a write to a configuration variable's row
// from anyone but a cluster owner or server code stamped for the write.
//
// Called by executeWrite before the stored row is read: the answer is a
// property of the concept and the caller, never of the payload, so a refused
// write reads nothing and cannot be partial.
func guardPlatformVariableWrite(ctx context.Context, conceptName string) error {
	if !platformVariableConcepts[conceptName] {
		return nil
	}
	if _, escaped := rowAuthzWriteEscape(ctx); escaped {
		return nil
	}
	holds := "holds no role"
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil && strings.TrimSpace(string(ac.Role)) != "" {
		holds = fmt.Sprintf("holds %q", string(ac.Role))
	}
	return fmt.Errorf(
		"%s: write refused -- a configuration variable's row is written by a cluster owner, or by "+
			"server code that has authorized its caller and stamps internal origin for the one write "+
			"(a settings save). The resolver reads a row by name ahead of the environment, so who "+
			"writes a row decides what its name means on every node. This caller %s; use the settings "+
			"surface that owns the value, or ask a cluster owner. See "+
			"component/memql/platform_variable_write_guard.go",
		conceptName, holds)
}
