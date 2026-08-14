package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (e *MemQLEngine) evaluateRelationshipExpression(ctx context.Context, expr *RelationshipExpression, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
	if expr == nil {
		return nil, fmt.Errorf("relationship expression is nil")
	}

	innerNodes, err := e.evaluateExpression(ctx, expr.Target, timestamp, limit, nil)
	if err != nil {
		return nil, err
	}
	if len(innerNodes) == 0 {
		return []memorynodes.MemoryNode{}, nil
	}

	var related []memorynodes.MemoryNode

	// label scopes the traversal to edges carrying that `as` domain label
	// (memql#3656). Empty is the unlabelled form -- follow every edge of the
	// type, which is what every traversal predating #3656 does -- and is
	// spelled as a nil filter rather than an empty one, because a filter
	// holding the empty string means "only edges that are unlabelled".
	label := strings.TrimSpace(expr.Label)
	var labels []string
	if label != "" {
		labels = []string{label}
	}

	switch expr.Function {
	case RelParentOf:
		related, err = e.resolveParentOf(ctx, innerNodes, timestamp, labels, limit)
	case RelChildOf:
		related, err = e.resolveChildOf(ctx, innerNodes, timestamp, labels, limit)
	case RelAliasOf:
		related, err = e.resolveAliasOrEquals(ctx, innerNodes, timestamp, relationshipTypeAlias, labels, limit)
	case RelEquals:
		related, err = e.resolveAliasOrEquals(ctx, innerNodes, timestamp, relationshipTypeEquals, labels, limit)
	case RelReferences:
		related, err = e.resolveReferences(ctx, innerNodes, timestamp, labels, limit)
	case RelCreatedBy:
		related, err = e.resolveCreatedBy(ctx, innerNodes, timestamp, labels, limit)
	case RelContains:
		related, err = e.resolveContains(ctx, innerNodes, timestamp, labels, limit)
	case RelOwns:
		related, err = e.resolveOwns(ctx, innerNodes, timestamp, labels, limit)
	case RelIds:
		// ids() projects the rows it is given; it follows no edge and reads no
		// relationship definition, so a label on it can only be a mistake.
		// Refusing beats ignoring: a silently-dropped label would be the
		// declaration theatre this epic exists to remove.
		if label != "" {
			err = fmt.Errorf("ids() follows no relationship, so it takes no label; drop %q", label)
			break
		}
		related, err = e.resolveIds(innerNodes, limit)
	default:
		err = fmt.Errorf("relationship function %q is not supported", expr.Function)
	}

	if err != nil {
		return nil, err
	}

	deduped := nodesToMap(related)
	result, err := mapToSortedSlice(deduped, nil)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (e *MemQLEngine) resolveContains(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, labels []string, limit int) ([]memorynodes.MemoryNode, error) {
	idSet := make(map[string]struct{})
	allowed := make(map[string]map[string]struct{})

	for _, node := range nodes {
		// Skip nodes with empty payloads (e.g., from ids() results)
		if len(node.Payload) == 0 {
			continue
		}

		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		defs := filterRelationshipDefinitions(e.relationshipDefinitionsForConcept(conceptMeta.Name), relationshipTypeContains, []string{relationshipDirectionOutgoing}, labels)
		if len(defs) == 0 {
			// A LABELLED traversal that matches no definition is an empty
			// answer, not an error (memql#3656): "no edge on this concept
			// means that label" is an ordinary question with an ordinary negative
			// answer. Only the UNLABELLED form still refuses -- asking for
			// this traversal on a concept that declares no such relationship
			// at all is a different mistake, and its behaviour is pinned by
			// TestRelationshipEmptyAnswer_ResolversDisagree (memql#3671).
			if labels != nil {
				continue
			}
			return nil, fmt.Errorf("concept %q does not define a 'contains' relationship required by contains()", conceptMeta.Name)
		}

		payloadMap, err := payloadToMap(node.Payload)
		if err != nil {
			return nil, fmt.Errorf("parse payload for node %q: %w", node.ID, err)
		}

		for _, def := range defs {
			targetConcept := strings.TrimSpace(def.TargetConcept)
			if targetConcept == "" {
				return nil, fmt.Errorf("relationship definition for concept %q is missing targetConcept", conceptMeta.Name)
			}

			path, err := splitRelationshipField(def.Field)
			if err != nil {
				return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", def.Field, conceptMeta.Name, err)
			}

			values, exists, err := extractStringArrayFromMap(payloadMap, path)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}

			for _, childId := range values {
				addAllowedConcept(allowed, childId, targetConcept)
				idSet[childId] = struct{}{}
				if limit > 0 && len(idSet) >= limit {
					break
				}
			}
		}
		if limit > 0 && len(idSet) >= limit {
			break
		}
	}

	if len(idSet) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
		if limit > 0 && len(ids) >= limit {
			break
		}
	}

	return e.fetchNodesByIds(ctx, ids, timestamp, allowed, limit)
}

func (e *MemQLEngine) resolveOwns(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, labels []string, limit int) ([]memorynodes.MemoryNode, error) {
	idSet := make(map[string]struct{})
	allowed := make(map[string]map[string]struct{})
	collected := make([]memorynodes.MemoryNode, 0)

	for _, node := range nodes {
		// Skip nodes with empty payloads (e.g., from ids() results)
		if len(node.Payload) == 0 {
			continue
		}

		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		defs := filterRelationshipDefinitions(e.relationshipDefinitionsForConcept(conceptMeta.Name), relationshipTypeOwns, nil, labels)
		if len(defs) == 0 {
			// A LABELLED traversal that matches no definition is an empty
			// answer, not an error (memql#3656): "no edge on this concept
			// means that label" is an ordinary question with an ordinary negative
			// answer. Only the UNLABELLED form still refuses -- asking for
			// this traversal on a concept that declares no such relationship
			// at all is a different mistake, and its behaviour is pinned by
			// TestRelationshipEmptyAnswer_ResolversDisagree (memql#3671).
			if labels != nil {
				continue
			}
			return nil, fmt.Errorf("concept %q does not define an 'owns' relationship required by owns()", conceptMeta.Name)
		}

		payloadMap, err := payloadToMap(node.Payload)
		if err != nil {
			return nil, fmt.Errorf("parse payload for node %q: %w", node.ID, err)
		}

		for _, def := range defs {
			targetConcept := strings.TrimSpace(def.TargetConcept)
			if targetConcept == "" {
				return nil, fmt.Errorf("relationship definition for concept %q is missing targetConcept", conceptMeta.Name)
			}

			path, err := splitRelationshipField(def.Field)
			if err != nil {
				return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", def.Field, conceptMeta.Name, err)
			}

			direction := strings.ToLower(strings.TrimSpace(def.Direction))

			if direction == relationshipDirectionOutgoing {
				values, exists, err := extractStringValueOrArrayFromMap(payloadMap, path)
				if err != nil {
					return nil, err
				}
				if exists {
					for _, ownedId := range values {
						addAllowedConcept(allowed, ownedId, targetConcept)
						idSet[ownedId] = struct{}{}
						if limit > 0 && len(idSet) >= limit {
							break
						}
					}
				}
			}

			if direction == relationshipDirectionIncoming {
				incoming, err := e.fetchNodesByJSONFieldValues(ctx, targetConcept, path, []string{strings.TrimSpace(node.ID)}, timestamp, limit)
				if err != nil {
					return nil, err
				}
				collected = append(collected, incoming...)
			}
			if limit > 0 && len(collected) >= limit {
				break
			}
		}
		if limit > 0 && len(collected) >= limit && len(idSet) >= limit {
			break
		}
	}

	if len(idSet) > 0 {
		ids := make([]string, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
			if limit > 0 && len(ids) >= limit {
				break
			}
		}

		nodes, err := e.fetchNodesByIds(ctx, ids, timestamp, allowed, limit)
		if err != nil {
			return nil, err
		}
		collected = append(collected, nodes...)
	}

	if len(collected) == 0 {
		return nil, nil
	}

	if limit > 0 && len(collected) > limit {
		collected = collected[:limit]
	}

	sorted, err := mapToSortedSlice(nodesToMap(collected), nil)
	if err != nil {
		return nil, err
	}
	return sorted, nil
}

func (e *MemQLEngine) resolveParentOf(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, labels []string, limit int) ([]memorynodes.MemoryNode, error) {
	parentTargets := make(map[string]map[string]struct{})

	for _, node := range nodes {
		// Skip nodes with empty payloads (e.g., from ids() results)
		if len(node.Payload) == 0 {
			continue
		}

		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		defs := e.relationshipDefinitionsForConcept(conceptMeta.Name)
		matches := filterRelationshipDefinitions(defs, relationshipTypeParent, []string{relationshipDirectionOutgoing}, labels)
		if len(matches) == 0 {
			// A LABELLED traversal that matches no definition is an empty
			// answer, not an error (memql#3656): "no edge on this concept
			// means that label" is an ordinary question with an ordinary negative
			// answer. Only the UNLABELLED form still refuses -- asking for
			// this traversal on a concept that declares no such relationship
			// at all is a different mistake, and its behaviour is pinned by
			// TestRelationshipEmptyAnswer_ResolversDisagree (memql#3671).
			if labels != nil {
				continue
			}
			return nil, fmt.Errorf("concept %q has no parent relationship definitions for parentOf", conceptMeta.Name)
		}

		payloadMap, err := payloadToMap(node.Payload)
		if err != nil {
			return nil, fmt.Errorf("parse payload for node %q: %w", node.ID, err)
		}

		for _, def := range matches {
			if strings.TrimSpace(def.TargetConcept) == "" {
				return nil, fmt.Errorf("relationship definition for concept %q is missing targetConcept", conceptMeta.Name)
			}

			path, err := splitRelationshipField(def.Field)
			if err != nil {
				return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", def.Field, conceptMeta.Name, err)
			}

			value, err := extractStringFromMap(payloadMap, path)
			if err != nil || strings.TrimSpace(value) == "" {
				continue // Skip nodes with missing parent pointers
			}

			parentId := strings.TrimSpace(value)
			addAllowedConcept(parentTargets, parentId, def.TargetConcept)
		}
		if limit > 0 && len(parentTargets) >= limit {
			break
		}
	}

	parentIds := keys(parentTargets)
	if len(parentIds) == 0 {
		// An empty traversal is an empty answer, not an error (memql#3671).
		// "Who is this row's parent" asked about a root row is an ordinary
		// question and "none" is its ordinary answer -- and parentOf is the
		// most-reached function on this surface, `parent` being 92 of the 141
		// corpus declarations. Failing the whole query instead only bit when NO
		// row in the set had a parent, which is why a mixed set hid it.
		//
		// memql#3656 had carved out the LABELLED case here; the family agrees
		// now, so the carve-out goes with it.
		return nil, nil
	}

	if limit > 0 && len(parentIds) > limit {
		parentIds = parentIds[:limit]
	}

	return e.fetchNodesByIds(ctx, parentIds, timestamp, parentTargets, limit)
}

func (e *MemQLEngine) resolveChildOf(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, labels []string, limit int) ([]memorynodes.MemoryNode, error) {
	type relationshipQueryKey struct {
		targetConcept string
		field         string
	}

	queries := make(map[relationshipQueryKey][]string)

	for _, node := range nodes {
		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		defs := e.relationshipDefinitionsForConcept(conceptMeta.Name)
		matches := filterRelationshipDefinitions(defs, relationshipTypeParent, []string{relationshipDirectionIncoming}, labels)
		if len(matches) == 0 {
			// A LABELLED traversal that matches no definition is an empty
			// answer, not an error (memql#3656): "no edge on this concept
			// means that label" is an ordinary question with an ordinary negative
			// answer. Only the UNLABELLED form still refuses -- asking for
			// this traversal on a concept that declares no such relationship
			// at all is a different mistake, and its behaviour is pinned by
			// TestRelationshipEmptyAnswer_ResolversDisagree (memql#3671).
			if labels != nil {
				continue
			}
			return nil, fmt.Errorf("concept %q has no child relationship definitions for childOf", conceptMeta.Name)
		}

		for _, def := range matches {
			if strings.TrimSpace(def.TargetConcept) == "" {
				return nil, fmt.Errorf("relationship definition for concept %q is missing targetConcept", conceptMeta.Name)
			}

			key := relationshipQueryKey{
				targetConcept: strings.TrimSpace(def.TargetConcept),
				field:         strings.TrimSpace(def.Field),
			}
			queries[key] = append(queries[key], strings.TrimSpace(node.ID))
			if limit > 0 && len(queries[key]) >= limit {
				break
			}
		}
		if limit > 0 && len(queries) >= limit {
			break
		}
	}

	if len(queries) == 0 {
		// A LABELLED traversal that found nothing is an empty answer, not an
		// error (memql#3656). The per-node filter above already skips a node
		// whose definitions carry no matching label; this SECOND, pre-existing
		// gate then refused the empty collection it produced, so four of the
		// eight functions still failed the whole query on a label miss.
		// An empty traversal is an empty answer, not an error (memql#3671), so
		// the labelled carve-out memql#3656 added here is now the behaviour for
		// both forms.
		return nil, nil
	}

	var results []memorynodes.MemoryNode

	for key, ids := range queries {
		path, err := splitRelationshipField(key.field)
		if err != nil {
			return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", key.field, key.targetConcept, err)
		}

		children, err := e.fetchNodesByJSONFieldValues(ctx, key.targetConcept, path, ids, timestamp, limit)
		if err != nil {
			return nil, err
		}
		results = append(results, children...)
		if limit > 0 && len(results) >= limit {
			break
		}
	}

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

func (e *MemQLEngine) resolveAliasOrEquals(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, relType string, labels []string, limit int) ([]memorynodes.MemoryNode, error) {
	type relationshipQueryKey struct {
		targetConcept string
		field         string
	}

	queries := make(map[relationshipQueryKey][]string)
	canonicalTargets := make(map[string]map[string]struct{})

	for _, node := range nodes {
		// Skip nodes with empty payloads (e.g., from ids() results)
		if len(node.Payload) == 0 {
			continue
		}

		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		defs := e.relationshipDefinitionsForConcept(conceptMeta.Name)
		matches := filterRelationshipDefinitions(defs, relType, nil, labels)
		if len(matches) == 0 {
			// A LABELLED traversal that matches no definition is an empty
			// answer, not an error (memql#3656): "no edge on this concept
			// means that label" is an ordinary question with an ordinary negative
			// answer. Only the UNLABELLED form still refuses -- asking for
			// this traversal on a concept that declares no such relationship
			// at all is a different mistake, and its behaviour is pinned by
			// TestRelationshipEmptyAnswer_ResolversDisagree (memql#3671).
			if labels != nil {
				continue
			}
			return nil, fmt.Errorf("concept %q has no %s relationship definitions", conceptMeta.Name, relType)
		}

		payloadMap, err := payloadToMap(node.Payload)
		if err != nil {
			return nil, fmt.Errorf("parse payload for node %q: %w", node.ID, err)
		}

		for _, def := range matches {
			path, err := splitRelationshipField(def.Field)
			if err != nil {
				return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", def.Field, conceptMeta.Name, err)
			}

			value, err := extractStringFromMap(payloadMap, path)
			if err != nil {
				continue // Skip nodes with missing relationship pointers
			}

			// A blank pointer is a MISSING pointer, skipped exactly like an
			// absent one. This used to fall back to node.ID, so a dangling
			// alias reported ITSELF as its own canonical target (memql#3671) --
			// a self-loop a client following aliasOf takes as a real edge.
			// `alias` means "this row is an alias FOR that one"; a row aliasing
			// itself is not a weaker answer than "no target", it is a different
			// and false one.
			//
			// It also put the two spellings of "no target" on opposite sides of
			// one branch: an absent key errored, a present-but-empty key
			// returned itself. Which mistake a writer happened to make decided
			// whether the caller got an error or a wrong answer.
			canonical := strings.TrimSpace(value)
			if canonical == "" {
				continue
			}

			targetConcept := strings.TrimSpace(def.TargetConcept)
			if targetConcept == "" {
				targetConcept = conceptMeta.Name
			}

			key := relationshipQueryKey{
				targetConcept: targetConcept,
				field:         strings.TrimSpace(def.Field),
			}
			queries[key] = append(queries[key], canonical)
			addAllowedConcept(canonicalTargets, canonical, targetConcept)
			addAllowedConcept(canonicalTargets, canonical, conceptMeta.Name)

			if limit > 0 && len(queries[key]) >= limit {
				break
			}
		}
		if limit > 0 && len(canonicalTargets) >= limit {
			break
		}
	}

	if len(queries) == 0 {
		// A LABELLED traversal that found nothing is an empty answer, not an
		// error (memql#3656). The per-node filter above already skips a node
		// whose definitions carry no matching label; this SECOND, pre-existing
		// gate then refused the empty collection it produced, so four of the
		// eight functions still failed the whole query on a label miss.
		// An empty traversal is an empty answer, not an error (memql#3671), so
		// the labelled carve-out memql#3656 added here is now the behaviour for
		// both forms. Reached when every node's pointer key is ABSENT; the
		// present-but-blank spelling is skipped above and lands here too, which
		// is the point -- both mean "points nowhere".
		return nil, nil
	}

	var related []memorynodes.MemoryNode
	for key, values := range queries {
		path, err := splitRelationshipField(key.field)
		if err != nil {
			return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", key.field, key.targetConcept, err)
		}

		nodes, err := e.fetchNodesByJSONFieldValues(ctx, key.targetConcept, path, values, timestamp, limit)
		if err != nil {
			return nil, err
		}
		related = append(related, nodes...)
		if limit > 0 && len(related) >= limit {
			break
		}
	}

	canonicalIds := keys(canonicalTargets)
	if len(canonicalIds) > 0 {
		if limit > 0 && len(canonicalIds) > limit {
			canonicalIds = canonicalIds[:limit]
		}
		canonicalNodes, err := e.fetchNodesByIds(ctx, canonicalIds, timestamp, canonicalTargets, limit)
		if err != nil {
			return nil, err
		}
		related = append(related, canonicalNodes...)
	}

	if limit > 0 && len(related) > limit {
		related = related[:limit]
	}

	return related, nil
}

func (e *MemQLEngine) resolveReferences(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, labels []string, limit int) ([]memorynodes.MemoryNode, error) {
	type relationshipQueryKey struct {
		targetConcept string
		field         string
	}

	incomingQueries := make(map[relationshipQueryKey][]string)
	outgoingTargets := make(map[string]map[string]struct{})

	for _, node := range nodes {
		// Skip nodes with empty payloads (e.g., from ids() results)
		if len(node.Payload) == 0 {
			continue
		}

		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		defs := e.relationshipDefinitionsForConcept(conceptMeta.Name)
		matches := filterRelationshipDefinitions(defs, relationshipTypeReferences, nil, labels)
		if len(matches) == 0 {
			// A LABELLED traversal that matches no definition is an empty
			// answer, not an error (memql#3656): "no edge on this concept
			// means that label" is an ordinary question with an ordinary negative
			// answer. Only the UNLABELLED form still refuses -- asking for
			// this traversal on a concept that declares no such relationship
			// at all is a different mistake, and its behaviour is pinned by
			// TestRelationshipEmptyAnswer_ResolversDisagree (memql#3671).
			if labels != nil {
				continue
			}
			return nil, fmt.Errorf("concept %q has no references relationship definitions", conceptMeta.Name)
		}

		var payloadMap map[string]any
		var payloadErr error

		for _, def := range matches {
			targetConcept := strings.TrimSpace(def.TargetConcept)
			if targetConcept == "" {
				targetConcept = conceptMeta.Name
			}

			path, err := splitRelationshipField(def.Field)
			if err != nil {
				return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", def.Field, conceptMeta.Name, err)
			}

			direction := strings.ToLower(strings.TrimSpace(def.Direction))

			if direction == relationshipDirectionOutgoing {
				if payloadMap == nil && payloadErr == nil {
					payloadMap, payloadErr = payloadToMap(node.Payload)
				}
				if payloadErr != nil {
					return nil, fmt.Errorf("parse payload for node %q: %w", node.ID, payloadErr)
				}

				value, err := extractStringFromMap(payloadMap, path)
				if err != nil || strings.TrimSpace(value) == "" {
					continue // Skip nodes with missing references pointers
				}
				targetId := strings.TrimSpace(value)
				addAllowedConcept(outgoingTargets, targetId, targetConcept)
				if limit > 0 && len(outgoingTargets) >= limit {
					break
				}
			}

			if direction == relationshipDirectionIncoming {
				key := relationshipQueryKey{
					targetConcept: targetConcept,
					field:         strings.TrimSpace(def.Field),
				}
				incomingQueries[key] = append(incomingQueries[key], strings.TrimSpace(node.ID))
				if limit > 0 && len(incomingQueries[key]) >= limit {
					break
				}
			}
		}
		if limit > 0 && len(outgoingTargets) >= limit && len(incomingQueries) >= limit {
			break
		}
	}

	var results []memorynodes.MemoryNode

	if len(outgoingTargets) > 0 {
		targetIds := keys(outgoingTargets)
		nodes, err := e.fetchNodesByIds(ctx, targetIds, timestamp, outgoingTargets, limit)
		if err != nil {
			return nil, err
		}
		results = append(results, nodes...)
		if limit > 0 && len(results) >= limit {
			results = results[:limit]
			return results, nil
		}
	}

	for key, ids := range incomingQueries {
		path, err := splitRelationshipField(key.field)
		if err != nil {
			return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", key.field, key.targetConcept, err)
		}

		nodes, err := e.fetchNodesByJSONFieldValues(ctx, key.targetConcept, path, ids, timestamp, limit)
		if err != nil {
			return nil, err
		}
		results = append(results, nodes...)
		if limit > 0 && len(results) >= limit {
			results = results[:limit]
			break
		}
	}

	return results, nil
}

func (e *MemQLEngine) resolveCreatedBy(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, labels []string, limit int) ([]memorynodes.MemoryNode, error) {
	type relationshipQueryKey struct {
		targetConcept string
		field         string
		fieldSource   string
	}

	incomingQueries := make(map[relationshipQueryKey][]string)
	outgoingTargets := make(map[string]map[string]struct{})

	for _, node := range nodes {
		// Skip nodes with empty payloads (e.g., from ids() results)
		if len(node.Payload) == 0 {
			continue
		}

		conceptMeta, err := e.conceptForNode(node)
		if err != nil {
			return nil, err
		}

		defs := e.relationshipDefinitionsForConcept(conceptMeta.Name)
		matches := filterRelationshipDefinitions(defs, relationshipTypeCreatedBy, nil, labels)
		if len(matches) == 0 {
			// A LABELLED traversal that matches no definition is an empty
			// answer, not an error (memql#3656): "no edge on this concept
			// means that label" is an ordinary question with an ordinary negative
			// answer. Only the UNLABELLED form still refuses -- asking for
			// this traversal on a concept that declares no such relationship
			// at all is a different mistake, and its behaviour is pinned by
			// TestRelationshipEmptyAnswer_ResolversDisagree (memql#3671).
			if labels != nil {
				continue
			}
			return nil, fmt.Errorf("concept %q has no createdBy relationship definitions", conceptMeta.Name)
		}

		var (
			payloadMap    map[string]any
			payloadErr    error
			payloadLoaded bool
		)

		for _, def := range matches {
			targetConcept := strings.TrimSpace(def.TargetConcept)
			if targetConcept == "" {
				targetConcept = conceptMeta.Name
			}

			direction := strings.ToLower(strings.TrimSpace(def.Direction))
			switch def.FieldSource {
			case memorynodes.FieldSourcePayload:
				if direction == relationshipDirectionOutgoing {
					if !payloadLoaded {
						payloadMap, payloadErr = payloadToMap(node.Payload)
						payloadLoaded = true
					}
					if payloadErr != nil {
						return nil, fmt.Errorf("parse payload for node %q: %w", node.ID, payloadErr)
					}

					path, err := splitRelationshipField(def.Field)
					if err != nil {
						return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", def.Field, conceptMeta.Name, err)
					}

					value, err := extractStringFromMap(payloadMap, path)
					if err != nil {
						continue // Skip nodes with missing createdBy pointers
					}
					targetId := strings.TrimSpace(value)
					if targetId != "" {
						addAllowedConcept(outgoingTargets, targetId, targetConcept)
						if limit > 0 && len(outgoingTargets) >= limit {
							break
						}
					}
				}

				if direction == relationshipDirectionIncoming {
					key := relationshipQueryKey{
						targetConcept: targetConcept,
						field:         strings.TrimSpace(def.Field),
						fieldSource:   def.FieldSource,
					}
					incomingQueries[key] = append(incomingQueries[key], strings.TrimSpace(node.ID))
					if limit > 0 && len(incomingQueries[key]) >= limit {
						break
					}
				}
			case memorynodes.FieldSourceTable:
				if direction == relationshipDirectionOutgoing {
					value, err := nodeFieldValue(node, def.Field)
					if err != nil {
						return nil, err
					}
					targetId := strings.TrimSpace(value)
					if targetId != "" {
						addAllowedConcept(outgoingTargets, targetId, targetConcept)
						if limit > 0 && len(outgoingTargets) >= limit {
							break
						}
					}
				}

				if direction == relationshipDirectionIncoming {
					key := relationshipQueryKey{
						targetConcept: targetConcept,
						field:         strings.TrimSpace(def.Field),
						fieldSource:   def.FieldSource,
					}
					incomingQueries[key] = append(incomingQueries[key], strings.TrimSpace(node.ID))
					if limit > 0 && len(incomingQueries[key]) >= limit {
						break
					}
				}
			default:
				return nil, fmt.Errorf("relationship fieldSource %q is invalid", def.FieldSource)
			}
		}
		if limit > 0 && len(outgoingTargets) >= limit && len(incomingQueries) >= limit {
			break
		}
	}

	var results []memorynodes.MemoryNode

	if len(outgoingTargets) > 0 {
		targetIds := keys(outgoingTargets)
		nodes, err := e.fetchNodesByIds(ctx, targetIds, timestamp, outgoingTargets, limit)
		if err != nil {
			return nil, err
		}
		results = append(results, nodes...)
		if limit > 0 && len(results) >= limit {
			results = results[:limit]
			return results, nil
		}
	}

	for key, ids := range incomingQueries {
		var nodes []memorynodes.MemoryNode
		var err error
		if key.fieldSource == memorynodes.FieldSourceTable {
			nodes, err = e.fetchNodesByNodeFieldValues(ctx, key.targetConcept, key.field, ids, timestamp, limit)
		} else {
			path, pathErr := splitRelationshipField(key.field)
			if pathErr != nil {
				return nil, fmt.Errorf("relationship field %q for concept %q is invalid: %w", key.field, key.targetConcept, pathErr)
			}
			nodes, err = e.fetchNodesByJSONFieldValues(ctx, key.targetConcept, path, ids, timestamp, limit)
		}
		if err != nil {
			return nil, err
		}
		results = append(results, nodes...)
		if limit > 0 && len(results) >= limit {
			results = results[:limit]
			break
		}
	}

	return results, nil
}

func (e *MemQLEngine) resolveIds(nodes []memorynodes.MemoryNode, limit int) ([]memorynodes.MemoryNode, error) {
	if len(nodes) == 0 {
		return nil, nil
	}

	result := make(map[string]memorynodes.MemoryNode, len(nodes))
	for _, node := range nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			continue
		}
		if _, exists := result[id]; exists {
			continue
		}

		stripped := node
		stripped.Schema = nil
		stripped.Payload = nil

		result[id] = stripped
		if limit > 0 && len(result) >= limit {
			break
		}
	}

	return mapToSortedSlice(result, nil)
}

// relationshipDefinitionsForConcept is the read side of e.relationships, which a
// concept promote rewrites on a running engine (memql#3746) -- hence the lock.
// See the conceptStateMu comment on MemQLEngine.
func (e *MemQLEngine) relationshipDefinitionsForConcept(name string) []RelationshipDefinition {
	e.conceptStateMu.RLock()
	defer e.conceptStateMu.RUnlock()
	if e.relationships.ByConcept == nil {
		return nil
	}
	return e.relationships.ByConcept[strings.TrimSpace(name)]
}

// setConceptRelationships publishes a freshly-derived relationship index. The
// only writer besides Init's own reset.
func (e *MemQLEngine) setConceptRelationships(byConcept map[string][]RelationshipDefinition) {
	e.conceptStateMu.Lock()
	defer e.conceptStateMu.Unlock()
	e.relationships = relationshipRegistry{ByConcept: byConcept}
}

// schemaIndex is the read side of e.schemaIdx, rebuilt by a concept promote for
// the same reason and under the same lock.
func (e *MemQLEngine) schemaIndex() *schemaIndex {
	e.conceptStateMu.RLock()
	defer e.conceptStateMu.RUnlock()
	return e.schemaIdx
}

// setSchemaIndex publishes a freshly-built schema index.
func (e *MemQLEngine) setSchemaIndex(idx *schemaIndex) {
	e.conceptStateMu.Lock()
	defer e.conceptStateMu.Unlock()
	e.schemaIdx = idx
}

// filterRelationshipDefinitions selects the definitions a traversal should
// follow: those of the given structural type, in one of the given directions
// (nil means any), and -- when label is non-empty -- carrying that `as` domain
// label (memql#3656).
//
// The label is compared to RelationshipDefinition.As, the open vocabulary
// #3652 introduced, so it is never checked against a list. Two consequences,
// both deliberate:
//
//   - An empty label matches every definition of the type. That is what keeps
//     the unlabelled form -- every traversal written before #3656 -- unchanged.
//   - Several definitions on one concept may share a label, and all of them
//     are returned, so the traversal follows their UNION. The per-field
//     duplicate rule (engine.go) already blocks the genuinely ambiguous case,
//     and the union is the useful reading of "every edge meaning assignedTo".
//
// A label matching nothing returns nil, which each resolver turns into an
// empty result rather than an error.
// relationshipLabelMatches reports whether a definition's `as` label is
// selected by a label filter (memql#3656).
//
// nil means UNSCOPED -- every label matches, which is what the one-argument
// traversal form means and what every traversal predating #3656 does. A
// non-nil filter matches exactly the labels it holds, and the empty string is
// a legitimate member of it: `[]string{""}` selects only the definitions that
// carry NO label, which is what graph expansion needs to attribute an edge to
// the right definition when a concept mixes labelled and unlabelled edges of
// one type.
func relationshipLabelMatches(as string, labels []string) bool {
	if labels == nil {
		return true
	}
	actual := strings.TrimSpace(as)
	for _, want := range labels {
		if actual == strings.TrimSpace(want) {
			return true
		}
	}
	return false
}

// relationshipLabelsPresent returns the distinct `as` labels across defs, in
// first-seen order, with the empty string included when any definition is
// unlabelled. Graph expansion walks these one at a time so every emitted edge
// can be attributed to the label that produced it (memql#3656) -- resolving
// the whole type at once would leave a concept's two differently-labelled
// edges indistinguishable on the wire, which is the case the label exists for.
func relationshipLabelsPresent(defs []RelationshipDefinition) []string {
	seen := make(map[string]struct{}, len(defs))
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		label := strings.TrimSpace(def.As)
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	return out
}

func filterRelationshipDefinitions(defs []RelationshipDefinition, relType string, directions []string, labels []string) []RelationshipDefinition {
	if len(defs) == 0 {
		return nil
	}

	targetType, ok := canonicalRelationshipType(relType)
	if !ok {
		return nil
	}

	var result []RelationshipDefinition
	for _, def := range defs {
		if def.Type != targetType {
			continue
		}
		if !relationshipLabelMatches(def.As, labels) {
			continue
		}
		if len(directions) == 0 {
			result = append(result, def)
			continue
		}
		dir := strings.ToLower(strings.TrimSpace(def.Direction))
		for _, allowed := range directions {
			if strings.EqualFold(dir, allowed) {
				result = append(result, def)
				break
			}
		}
	}
	return result
}

func splitRelationshipField(field string) ([]string, error) {
	if strings.TrimSpace(field) == "" {
		return nil, fmt.Errorf("relationship field is required")
	}

	parts := strings.Split(field, ".")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			return nil, fmt.Errorf("relationship field %q is invalid", field)
		}
		result = append(result, trimmed)
	}
	return result, nil
}
