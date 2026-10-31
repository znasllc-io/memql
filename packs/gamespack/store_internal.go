package gamespack

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// store_internal.go -- the writers that stamp INTERNAL ORIGIN: createGamesPlanTrace,
// createGamesSkill and recordGamesSkillLadder.
//
// Each stamp is on one rendered call of one @serverOnly construct, named literally so the
// server-only caller gate (test/dslconformance) sees it. The actor is never touched, so
// ownerUserId is stamped from the caller. INTERNAL ORIGIN ESCAPES THE OWNED TIER'S WRITE
// GUARD (rowAuthzWriteEscape): the tier does not compare the owner of a row a write lands
// on, so which rows a caller may write rests on the callers of these writers --
// storeTrace refuses a trace id another owner holds, a skill's id is derived from its
// owner's key (SkillID), and moveLadder moves only a skill the caller owns, the owner
// compared in the canonical spelling after a fresh read inside the skill's gate. The
// marked context is a local of the one call and is never returned: the
// call_origin_conformance_test.go entry for this package rests on that, and
// internal_origin_test.go holds it to the source -- exactly these three stamps, each
// inline as the context of the call that runs the mutation.

func (s engineStore) createTrace(ctx context.Context, args map[string]any) error {
	call, err := langparser.RenderCall("createGamesPlanTrace", compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("games: mutation createGamesPlanTrace: %w", err)
	}
	return nil
}

func (s engineStore) createSkill(ctx context.Context, args map[string]any) error {
	call, err := langparser.RenderCall("createGamesSkill", compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("games: mutation createGamesSkill: %w", err)
	}
	return nil
}

func (s engineStore) recordLadder(ctx context.Context, args map[string]any) error {
	call, err := langparser.RenderCall("recordGamesSkillLadder", compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return fmt.Errorf("games: mutation recordGamesSkillLadder: %w", err)
	}
	return nil
}
