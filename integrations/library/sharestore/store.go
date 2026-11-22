// Package sharestore limits internal origin to the four file-sharing writes
// (file sharing, 2026-11-18).
//
// v1:library:share and v1:library:shareNotice are @serverWritten, so every
// write to them needs internal origin, and the four mutations that write them
// are @serverOnly. The stamp lives here, in a package that holds nothing else,
// so the allowlist entry in call_origin_conformance_test.go names exactly what
// it admits:
//
//   - ONLY THE FOUR WRITES. Any other construct is refused before the engine is
//     reached.
//   - NO WRITE NAMES AN OWNER. Each mutation stamps ownerUserId from the actor
//     already on ctx -- the sharer for a share, the recipient (borrowed for that
//     one write by integrations/library/share.go) for a notice -- so a share is
//     always its sharer's and a notice always its recipient's.
//   - THE STAMP DIES IN ONE EXECUTE. No function returns a context.
//   - NO READS. Every decision -- the caller owns the file, the recipient is in
//     their directory, the lifetime is one of the four, a share being ended is
//     the caller's -- is made by integrations/library/share.go under the
//     original caller before it gets here. That ordering matters for the two
//     ENDS: internal origin skips the write guard's owner check on an update,
//     so share.go reads the share back under its sharer first.
//
// Preconditions asserted in store_test.go; the acts across ranks in
// integrations/library/share_db_test.go.
package sharestore

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Writes are the only constructs this package stamps.
var Writes = []string{"putLibraryShare", "endLibraryShare", "putLibraryShareNotice", "endLibraryShareNotice"}

// Write runs one of the four sharing mutations under the actor on ctx, at
// internal origin for that one call.
func Write(ctx context.Context, engine memql.IntegrationEngineAccess, name string, args map[string]any) error {
	allowed := false
	for _, w := range Writes {
		allowed = allowed || w == name
	}
	if !allowed {
		return fmt.Errorf("sharestore: %q is not a file-sharing write", name)
	}
	if _, named := args["ownerUserId"]; named {
		return fmt.Errorf("sharestore: %s stamps ownerUserId from the actor; a caller may not name it", name)
	}
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" {
		return fmt.Errorf("sharestore: %s needs the person it is written for", name)
	}
	if engine == nil {
		return fmt.Errorf("sharestore: engine not configured")
	}
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return err
	}
	if _, err := engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
