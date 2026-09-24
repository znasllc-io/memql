package memql

import (
	"context"
	"fmt"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// MayWriteRow answers, without writing, the question executeWrite's row-authz
// guard asks of a write onto an existing row: would it admit THIS caller's
// write to (concept, id)? Connect Shopify asks it before any of its writes
// (integrations/shopify, ResolveConnectSite), because "may read" and "may
// write" are two rules over one resolution and only the write one is the gate
// its writes run behind.
//
// It answers the ROW-AUTHZ WRITE GUARD, and only that: the same stored row, the
// same guardRowAuthzWrite, with the rank and account scopes installed exactly as
// executeWrite installs them. A caller that skipped the scopes would be refused
// every row the account grant admits, and the answer would disagree with the
// write it stands in front of.
//
// executeWrite asks more than this of a write onto an existing row: the
// organization boundary's checks (validateOrganizationSensitiveChanges and
// validateOrganizationTransfer, memql#5598) judge what the write CHANGES, which
// a question with no payload cannot answer. Those are asked separately, by the
// write itself and, where a caller needs the answer first, through a question
// of their own -- Connect Shopify asks the organization boundary for the store
// part through MayChangeStoreBinding in step 8 (integrations/shopify,
// personMayConnect), before any of its writes.
//
// A row that does not exist is not writable -- the update contract's refusal.
// An error is a read that failed, never a "no".
func (e *MemQLEngine) MayWriteRow(ctx context.Context, conceptName, id string) (bool, error) {
	if !e.canResolve() {
		return false, ErrEngineNotInitialized
	}
	conceptMeta, err := e.concepts.Get(conceptName)
	if err != nil {
		return false, fmt.Errorf("concept %q not found: %w", conceptName, err)
	}
	prior, existed, err := e.loadPriorPayload(ctx, conceptMeta, id)
	if err != nil {
		return false, err
	}
	ctx = contextWithAccountScopeMemo(contextWithRankScopeMemo(ctx, e), e)
	return guardRowAuthzWrite(ctx, conceptMeta.Name, id, prior, existed, true) == nil, nil
}

// MayReadConcept answers the row-authz READ admission for a concept whose tier
// decides from the caller alone: `public`, and `clusterOwner` with or without
// its read floor (memql#5216), whose injected predicate folds to a constant. So
// "may this caller read a row of this concept" has an answer before any row
// exists, and it is the answer the read gives once one does -- the row gate's
// own arm (rowAuthzAdmitsMode), with the rank memo installed as a read installs
// it, so the floor resolves through the same ladder.
//
// Connect Shopify asks it on a FIRST Connect (integrations/shopify,
// personMayConnect): there is no store row to read back yet, and the attach at
// step 14 will ask the store's read tier of the person once step 12 has made
// one. Asked only then, the credentials would be written before the refusal.
//
// A tier that compares a row (owned, granted), an undeclared concept and one
// with an organization boundary have no row-free answer, and asking is an
// error, never a guess.
func (e *MemQLEngine) MayReadConcept(ctx context.Context, conceptName string) (bool, error) {
	if !e.canResolve() {
		return false, ErrEngineNotInitialized
	}
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil || HasOrganizationBoundary(conceptName) ||
		(decl.Tier != langparser.RowAuthzPublic && decl.Tier != langparser.RowAuthzClusterOwner) {
		return false, fmt.Errorf("row-authz: whether %s is readable depends on the row, so there is no answer without one", conceptName)
	}
	return rowAuthzAdmitsMode(contextWithRankScopeMemo(ctx, e), conceptName, "", nil, false) == rowAuthzAdmit, nil
}
