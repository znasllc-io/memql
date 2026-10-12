// Package libraryfile is the graph writer for a NEW Library file row -- and,
// deliberately, the smallest possible home for the internal-origin stamp
// createLibraryFile needs.
//
// createLibraryFile is @serverOnly, and blobUrl is why: it is the storage path
// every reader of the file streams with the NODE's storage credential, so a
// client that could author it could point a row it owns at somebody else's
// object and read it back through GET /artifacts/{id}/content. The server's
// writers compose the path themselves -- library/{owner}/{fileId}/{name} --
// from the verified actor and an engine-minted file id, after writing the
// bytes there.
//
// Satisfying @serverOnly from the upload route means stamping
// auth.ContextWithInternalOrigin on a REQUEST-DERIVED context, which is the
// shape component/auth/call_origin.go warns about. So the stamp lives here,
// in a package that contains nothing else, with these properties asserted by
// store_internal_origin_test.go and the package named in the root
// call-origin allowlist -- the same arrangement as its siblings
// component/server/uploadsession and component/server/fileversion:
//
//   - the stamped context is a local, derived per call, and no method returns
//     a context -- the stamp cannot outlive one write;
//   - no write names ownerUserId or status -- the mutation stamps both from
//     the actor already on the caller's context, so the row is the caller's;
//   - THE KEY IS THE ACTOR'S OWN. Before anything is executed, blobUrl must
//     sit under the actor's library/{owner}/ prefix (core/librarykey). The
//     stamp opens @serverOnly for one call; this is what keeps a value a
//     caller chose -- forwarded by a future writer that should not have --
//     from riding through it;
//   - there are NO READS here. Every Library read runs unstamped under the
//     caller's actor, so row admission stays the whole authorization.
//
// Internal origin also skips the write guard's owner check on an existing
// row, which is why createLibraryFile declares ownerUserId and blobUrl
// @createOnly: a later create onto an existing id cannot re-own or re-point
// it. Every writer mints its file id (or derives it from something only its
// caller owns), so that is a backstop rather than the mechanism.
package libraryfile

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/librarykey"
)

// Executor is the one engine capability this store needs. Declared here so
// the package imports nothing from component/server (which imports it).
type Executor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Store writes new v1:library:file rows.
type Store struct {
	engine Executor
}

// NewStore creates a file store over an engine executor.
func NewStore(engine Executor) *Store {
	return &Store{engine: engine}
}

// stampedFields are the two values createLibraryFile stamps rather than
// accepts. Neither is declared as an argument, so the engine would silently
// drop one -- and a call site passing either believes it wrote something the
// mutation never let it write. Refused here instead, by name.
var stampedFields = []string{"ownerUserId", "status"}

// Create runs createLibraryFile with args, the mutation's own arguments, under
// the actor on ctx. It refuses -- before the engine is reached -- a call with
// no actor, one naming a field the mutation stamps, and one whose blobUrl is
// not under the actor's own library/{owner}/ prefix.
func (s *Store) Create(ctx context.Context, args map[string]any) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("engine not configured")
	}
	access, _ := auth.AccessFromContext(ctx)
	owner := ""
	if access != nil {
		owner = strings.TrimSpace(access.UserId)
	}
	if owner == "" {
		return fmt.Errorf("a Library file needs a signed-in owner to be created for")
	}
	for _, field := range stampedFields {
		if _, named := args[field]; named {
			return fmt.Errorf("createLibraryFile stamps %s from the actor; a caller may not name it", field)
		}
	}
	blobUrl, _ := args["blobUrl"].(string)
	if err := librarykey.Check(blobUrl, owner, ""); err != nil {
		return fmt.Errorf("refusing a Library file whose blobUrl is not the creator's own: %w", err)
	}
	call, err := langparser.RenderCall("createLibraryFile", args)
	if err != nil {
		return err
	}
	// THE STAMP: a derived local, handed to the one Execute and never stored
	// or returned.
	if _, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("execute createLibraryFile: %w", err)
	}
	return nil
}
