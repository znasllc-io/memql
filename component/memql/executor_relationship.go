package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
)

func (e *MemQLEngine) evaluateRelationshipExpression(ctx context.Context, expr *RelationshipExpression, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
	if expr == nil {
		return nil, fmt.Errorf("relationship expression is nil")
	}

	innerNodes, err := e.evaluateExpression(ctx, expr.Target, timestamp, limit, 0, nil)
	if err != nil {
		return nil, err
	}
	if len(innerNodes) == 0 {
		return []memorynodes.MemoryNode{}, nil
	}

	var related []memorynodes.MemoryNode

	switch expr.Function {
	case RelParentOf:
		related, err = e.resolveParentOf(ctx, innerNodes, timestamp, limit)
	case RelChildOf:
		related, err = e.resolveChildOf(ctx, innerNodes, timestamp, limit)
	case RelAliasOf:
		related, err = e.resolveAliasOrEquals(ctx, innerNodes, timestamp, relationshipTypeAlias, limit)
	case RelEquals:
		related, err = e.resolveAliasOrEquals(ctx, innerNodes, timestamp, relationshipTypeEquals, limit)
	case RelInteractsWith:
		related, err = e.resolveInteractsWith(ctx, innerNodes, timestamp, limit)
	case RelCreatedBy:
		related, err = e.resolveCreatedBy(ctx, innerNodes, timestamp, limit)
	case RelContains:
		related, err = e.resolveContains(ctx, innerNodes, timestamp, limit)
	case RelOwns:
		related, err = e.resolveOwns(ctx, innerNodes, timestamp, limit)
	case RelIds:
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

func (e *MemQLEngine) resolveContains(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
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

		defs := filterRelationshipDefinitions(e.relationshipDefinitionsForConcept(conceptMeta.Name), relationshipTypeContains, []string{relationshipDirectionOutgoing, relationshipDirectionBidirectional})
		if len(defs) == 0 {
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

func (e *MemQLEngine) resolveOwns(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
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

		defs := filterRelationshipDefinitions(e.relationshipDefinitionsForConcept(conceptMeta.Name), relationshipTypeOwns, nil)
		if len(defs) == 0 {
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

			if direction == relationshipDirectionOutgoing || direction == relationshipDirectionBidirectional {
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

			if direction == relationshipDirectionIncoming || direction == relationshipDirectionBidirectional {
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

func (e *MemQLEngine) resolveParentOf(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
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
		matches := filterRelationshipDefinitions(defs, relationshipTypeParent, []string{relationshipDirectionOutgoing, relationshipDirectionBidirectional})
		if len(matches) == 0 {
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
		return nil, fmt.Errorf("parentOf traversal produced no parent references")
	}

	if limit > 0 && len(parentIds) > limit {
		parentIds = parentIds[:limit]
	}

	return e.fetchNodesByIds(ctx, parentIds, timestamp, parentTargets, limit)
}

func (e *MemQLEngine) resolveChildOf(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
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
		matches := filterRelationshipDefinitions(defs, relationshipTypeParent, []string{relationshipDirectionIncoming, relationshipDirectionBidirectional})
		if len(matches) == 0 {
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
		return nil, fmt.Errorf("childOf traversal produced no candidate queries")
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

func (e *MemQLEngine) resolveAliasOrEquals(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, relType string, limit int) ([]memorynodes.MemoryNode, error) {
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
		matches := filterRelationshipDefinitions(defs, relType, nil)
		if len(matches) == 0 {
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

			canonical := strings.TrimSpace(value)
			if canonical == "" {
				canonical = strings.TrimSpace(node.ID)
			}
			if canonical == "" {
				continue // Skip nodes with missing identifier
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
		return nil, fmt.Errorf("%s traversal produced no candidate queries", relType)
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

func (e *MemQLEngine) resolveInteractsWith(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
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
		matches := filterRelationshipDefinitions(defs, relationshipTypeInteracts, nil)
		if len(matches) == 0 {
			return nil, fmt.Errorf("concept %q has no interactsWith relationship definitions", conceptMeta.Name)
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

			if direction == relationshipDirectionOutgoing || direction == relationshipDirectionBidirectional {
				if payloadMap == nil && payloadErr == nil {
					payloadMap, payloadErr = payloadToMap(node.Payload)
				}
				if payloadErr != nil {
					return nil, fmt.Errorf("parse payload for node %q: %w", node.ID, payloadErr)
				}

				value, err := extractStringFromMap(payloadMap, path)
				if err != nil || strings.TrimSpace(value) == "" {
					continue // Skip nodes with missing interactsWith pointers
				}
				targetId := strings.TrimSpace(value)
				addAllowedConcept(outgoingTargets, targetId, targetConcept)
				if limit > 0 && len(outgoingTargets) >= limit {
					break
				}
			}

			if direction == relationshipDirectionIncoming || direction == relationshipDirectionBidirectional {
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

func (e *MemQLEngine) resolveCreatedBy(ctx context.Context, nodes []memorynodes.MemoryNode, timestamp *time.Time, limit int) ([]memorynodes.MemoryNode, error) {
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
		matches := filterRelationshipDefinitions(defs, relationshipTypeCreatedBy, nil)
		if len(matches) == 0 {
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
				if direction == relationshipDirectionOutgoing || direction == relationshipDirectionBidirectional {
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

				if direction == relationshipDirectionIncoming || direction == relationshipDirectionBidirectional {
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
				if direction == relationshipDirectionOutgoing || direction == relationshipDirectionBidirectional {
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

				if direction == relationshipDirectionIncoming || direction == relationshipDirectionBidirectional {
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

func (e *MemQLEngine) relationshipDefinitionsForConcept(name string) []RelationshipDefinition {
	if e.relationships.ByConcept == nil {
		return nil
	}
	return e.relationships.ByConcept[strings.TrimSpace(name)]
}

func filterRelationshipDefinitions(defs []RelationshipDefinition, relType string, directions []string) []RelationshipDefinition {
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

