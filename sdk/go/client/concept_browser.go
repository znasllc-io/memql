package client

import (
	"context"
	"fmt"
)

// BrowseConcept returns every row stored under the given concept id.
//
// Bypasses the named-primitive surface intentionally. The Cockpit's
// concept-browser tab is the canonical caller: it iterates the
// concept registry from ListConcepts and lets an operator click into
// any concept's rows without knowing its name at compile time. That
// use case is concept-name-agnostic by definition, so there's no
// equivalent named primitive in the DSL tree.
//
// Every OTHER caller should reach for a typed generated method on
// QueryClient. Direct concept browsing is reserved for admin /
// debug surfaces -- the named-primitive rule (see sdk/go/CLAUDE.md)
// applies to product code without exception.
func (qc *QueryClient) BrowseConcept(ctx context.Context, conceptId string) (*Result, error) {
	if conceptId == "" {
		return nil, fmt.Errorf("BrowseConcept: conceptId is required")
	}
	payload, err := qc.executeRaw(ctx, fmt.Sprintf("concept==%s", conceptId))
	if err != nil {
		return nil, fmt.Errorf("BrowseConcept(%s): %w", conceptId, err)
	}
	return &Result{payload: payload}, nil
}

// GetRowByConceptAndId returns the single row matching (conceptId,
// rowId). Same admin-surface caveat as BrowseConcept -- product code
// should use a typed lookup primitive (queryUserById, queryAgentById,
// etc.) instead.
func (qc *QueryClient) GetRowByConceptAndId(ctx context.Context, conceptId, rowId string) (*Result, error) {
	if conceptId == "" {
		return nil, fmt.Errorf("GetRowByConceptAndId: conceptId is required")
	}
	if rowId == "" {
		return nil, fmt.Errorf("GetRowByConceptAndId: rowId is required")
	}
	payload, err := qc.executeRaw(ctx, fmt.Sprintf("concept==%s;id==%s", conceptId, rowId))
	if err != nil {
		return nil, fmt.Errorf("GetRowByConceptAndId(%s, %s): %w", conceptId, rowId, err)
	}
	return &Result{payload: payload}, nil
}
