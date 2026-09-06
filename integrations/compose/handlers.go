package compose

import (
	"context"
	"fmt"
	"strings"
	"time"

	pure "github.com/znasllc-io/memql/component/compose"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// handlers.go -- the four capabilities that are not `materialize`.

// handleRunRecipe resolves a recipe's SELECTORS at this instant and
// materializes from them.
//
// THE SELECTORS ARE RESOLVED NOW, NOT REPLAYED. That is the difference
// between "make this again" and "make a copy of that", and it is why the
// recipe stores a selection rather than the rows the selection returned
// last time. Everything runs under the caller's own actor, so a recipe
// can never widen what its runner may read -- which also means a recipe
// shared with somebody produces THEIR rows, not its author's.
func (i *Integration) handleRunRecipe(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	recipeId := strings.TrimSpace(stringOf(args["recipeId"]))
	if recipeId == "" {
		return nil, fmt.Errorf("compose: runRecipe needs a recipeId")
	}
	row, err := i.store().recipeById(ctx, recipeId)
	if err != nil {
		return nil, fmt.Errorf("compose: reading the recipe: %w", err)
	}
	if row == nil {
		return nil, fmt.Errorf("compose: no recipe with that id is readable by you")
	}
	if boolOf(row["archived"]) {
		// REFUSED RATHER THAN RUN. An archived recipe is one somebody
		// retired, and running it would produce a file they would then
		// have to explain.
		return nil, fmt.Errorf("compose: that recipe is archived -- restore it first if you meant to run it")
	}

	format, err := pure.ParseFormat(stringOf(row["format"]))
	if err != nil {
		return nil, fmt.Errorf("compose: the recipe's format is not one this cluster can write: %w", err)
	}
	refs, err := selectorsToSources(row["sourceSelectors"])
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(stringOf(args["name"]))
	if name == "" {
		name = fmt.Sprintf("%s %s", stringOf(row["name"]), i.clock().UTC().Format("2006-01-02"))
	}

	out, err := i.materialize(ctx, ac.UserId, ac.PrimaryEmail, materializeArgs{
		Name:       name,
		Statement:  stringOf(row["description"]),
		Format:     format,
		Sources:    refs,
		TemplateId: strings.TrimSpace(stringOf(row["templateId"])),
		FolderId:   strings.TrimSpace(stringOf(row["folderId"])),
		AccountIds: stringList(row["accountIds"]),
		RecipeId:   recipeId,
	})
	if err != nil {
		return nil, err
	}
	out["recipeId"] = recipeId
	return i.resultNode(out), nil
}

// selectorsToSources maps a recipe's stored selectors onto the source
// shape materialize takes.
//
// A SELECTOR IS NOT A SOURCE, and the difference is the point of a
// recipe: a `concept_query` selector is a standing question that
// resolves to whatever matches NOW, rather than to the rows that matched
// when the recipe was saved.
//
// THERE IS NO `library_folder` SELECTOR, and its absence is deliberate
// rather than unfinished. "Everything in that folder" is exactly the
// standing selection a recipe wants, and the Library has no read that
// answers it: `v1:library:file.folderId` is the INITIAL FILING ONLY --
// the artifact index's own folderId becomes authoritative the moment
// anything is moved (dsl/library/concepts.memql says so on the field) --
// so a query built on the file row would silently omit every file
// somebody had since filed somewhere else. A recipe that quietly
// composes from a subset is worse than one that cannot express the
// selection, so the kind is refused by name and a `concept_query`
// selector over the artifact index is the way to say it today.
func selectorsToSources(raw any) ([]SourceRef, error) {
	items, ok := raw.([]any)
	if !ok {
		if typed, ok2 := raw.([]map[string]any); ok2 {
			items = make([]any, 0, len(typed))
			for _, m := range typed {
				items = append(items, m)
			}
		} else if raw == nil {
			return nil, fmt.Errorf("compose: that recipe names no sources, so running it would compose from nothing")
		} else {
			return nil, fmt.Errorf("compose: the recipe's sourceSelectors are not a list")
		}
	}
	out := make([]SourceRef, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("compose: sourceSelectors[%d] is not an object", idx)
		}
		kind := strings.TrimSpace(stringOf(m["kind"]))
		selector := strings.TrimSpace(stringOf(m["selector"]))
		label := strings.TrimSpace(stringOf(m["label"]))
		if selector == "" {
			return nil, fmt.Errorf("compose: sourceSelectors[%d] (%s) names nothing", idx, kind)
		}
		switch kind {
		case "concept_query", "query":
			out = append(out, SourceRef{Kind: KindQuery, Ref: selector, Label: label})
		case "library_file":
			out = append(out, SourceRef{Kind: KindLibraryFile, Ref: selector, Label: label})
		case "library_folder":
			return nil, fmt.Errorf("compose: sourceSelectors[%d] names a folder, and there is no Library read that answers %q -- a file's folderId is its initial filing only, so a folder selector would silently omit every file since moved. Name a concept_query over the artifact index instead", idx, selector)
		default:
			return nil, fmt.Errorf("compose: sourceSelectors[%d] declares kind %q (expected concept_query or library_file)", idx, kind)
		}
	}
	return out, nil
}

// handleCancel asks a composition to stop.
//
// THE VERB FLAGS AND ENDS NOTHING, which is the work spine's own
// contract and is restated here rather than shared because the reason
// matters at this surface: a render already in flight finishes and is
// journaled, so a transcript can never claim a composition stopped while
// its bytes were still being written somewhere.
func (i *Integration) handleCancel(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	compositionId := strings.TrimSpace(stringOf(args["compositionId"]))
	if compositionId == "" {
		return nil, fmt.Errorf("compose: cancel needs a compositionId")
	}
	st := i.store()
	row, err := st.compositionById(ctx, compositionId)
	if err != nil {
		return nil, fmt.Errorf("compose: reading the composition: %w", err)
	}
	if row == nil {
		return nil, fmt.Errorf("compose: no composition with that id is readable by you")
	}
	status := strings.TrimSpace(stringOf(row["status"]))
	if status == "ready" || status == "failed" || status == "cancelled" {
		// A TERMINAL COMPOSITION IS REPORTED, NOT RE-MARKED. Flipping a
		// `ready` row to `cancelled` would erase the fact that it
		// produced a file, and the file would still be there.
		return i.resultNode(map[string]any{
			"compositionId": compositionId,
			"status":        status,
			"cancelled":     false,
			"note":          "that composition had already finished, so nothing was stopped",
		}), nil
	}
	reason := strings.TrimSpace(stringOf(args["reason"]))
	if reason == "" {
		reason = "cancelled"
	}
	if err := st.updateCompositionState(ctx, map[string]any{
		"compositionId": compositionId,
		"status":        "cancelled",
		"failureReason": reason,
	}); err != nil {
		return nil, fmt.Errorf("compose: recording the cancellation: %w", err)
	}
	return i.resultNode(map[string]any{
		"compositionId": compositionId,
		"status":        "cancelled",
		"cancelled":     true,
	}), nil
}

// handleComposableConcepts answers what is worth composing from.
func (i *Integration) handleComposableConcepts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	includeUnmarked := boolOf(args["includeUnmarked"])
	concepts := i.composables(includeUnmarked)
	rows := make([]map[string]any, 0, len(concepts))
	markedCount := 0
	for _, c := range concepts {
		if c.Marked {
			markedCount++
		}
		rows = append(rows, map[string]any{
			"id": c.Id, "as": c.As, "fields": c.Fields, "list": c.List,
			"description": c.Description, "marked": c.Marked,
		})
	}
	return i.resultNode(map[string]any{
		"concepts": rows,
		"marked":   markedCount,
		// `registryAvailable` distinguishes "nothing is marked" from
		// "this node cannot see the registry". They look identical from
		// an empty list, and only one of them is something an operator
		// can fix.
		"registryAvailable": i.conceptsRef() != nil,
	}), nil
}

// handleResolveSources reports what a source list finds, having composed
// nothing.
//
// IT EXISTS SO SOMEBODY SEES AN EMPTY SELECTION BEFORE THEY SPEND A
// MODEL CALL DISCOVERING IT. Every read runs under the caller's own
// actor, so the count is of rows they can see and an id somebody pasted
// buys them nothing.
func (i *Integration) handleResolveSources(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	refs, err := parseSourceRefs(args["sources"])
	if err != nil {
		return nil, err
	}
	resolved, err := i.resolve(ctx, refs)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(resolved))
	total := 0
	for _, r := range resolved {
		total += r.Count
		rows = append(rows, map[string]any{
			"kind":  string(r.Ref.Kind),
			"ref":   r.Ref.Ref,
			"label": r.Ref.Label,
			"count": r.Count,
			// `problem` is EMPTY for a source that found nothing
			// because nothing matched -- that is an answer, and a
			// different one from "you cannot read that". The app draws
			// them differently and would have no way to if they shared
			// a spelling.
			"problem":    r.Problem,
			"capturedAt": r.CapturedAt.Format(time.RFC3339),
		})
	}
	return i.resultNode(map[string]any{"sources": rows, "total": total}), nil
}

func boolOf(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
