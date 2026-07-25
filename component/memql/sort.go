package memql

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type (
	sortFieldKind int
	sortValueKind int
)

const (
	sortFieldCreatedAt sortFieldKind = iota
	sortFieldCreatedBy
	sortFieldId
	sortFieldConcept
	sortFieldType
	sortFieldPayload
)

const (
	sortValueTime sortValueKind = iota
	sortValueString
	sortValueNumber
	sortValueBool
)

type compiledSort struct {
	fields           []compiledSortField
	signature        string
	requiresFullScan bool
}

type compiledSortField struct {
	kind        sortFieldKind
	direction   SortDirection
	payloadPath []string
	name        string
}

type sortComparable struct {
	present   bool
	direction SortDirection
	kind      sortValueKind
	str       string
	num       float64
	boolVal   bool
	timeVal   time.Time
}

func compileSortFields(spec []SortField) (*compiledSort, error) {
	normalized := spec
	if len(normalized) == 0 {
		normalized = []SortField{{Field: "createdAt", Direction: SortDirectionDesc}}
	}

	cs := &compiledSort{
		fields: make([]compiledSortField, 0, len(normalized)+2),
	}

	signatureParts := make([]string, 0, len(normalized))

	for idx, field := range normalized {
		compiled, err := compileSortField(field)
		if err != nil {
			return nil, fmt.Errorf("sort[%d]: %w", idx, err)
		}
		if compiled.kind == sortFieldPayload {
			cs.requiresFullScan = true
		}
		cs.fields = append(cs.fields, compiled)
		signatureParts = append(signatureParts, fmt.Sprintf("%s:%s", compiled.name, strings.ToLower(string(compiled.direction))))
	}

	// Append deterministic fallbacks so results stay stable even when the caller omits them.
	cs.fields = append(cs.fields, compiledSortField{
		kind:      sortFieldCreatedAt,
		direction: SortDirectionDesc,
		name:      "createdAt",
	})
	cs.fields = append(cs.fields, compiledSortField{
		kind:      sortFieldId,
		direction: SortDirectionAsc,
		name:      "id",
	})

	if len(signatureParts) == 0 {
		signatureParts = []string{"createdAt:desc"}
	}
	cs.signature = strings.Join(signatureParts, ",")

	return cs, nil
}

func compileSortField(field SortField) (compiledSortField, error) {
	name := strings.TrimSpace(field.Field)
	if name == "" {
		return compiledSortField{}, fmt.Errorf("field is required")
	}

	direction := normalizeSortDirection(field.Direction)

	// The `row.` namespace names a row intrinsic here for the same reason it
	// does in a filter (memql#2779): without it, `sort "row.createdAt"` fell
	// through to the bare-payload branch below and ordered on the JSONB path
	// {row,createdAt}, which no stored row carries -- a silent no-op ordering
	// rather than an error. Resolving it here keeps the two surfaces honest:
	// an author taught `row.` by the filter rule writes it in a sort key too.
	resolved, err := rowIntrinsicSortField(name)
	if err != nil {
		return compiledSortField{}, err
	}
	if resolved != "" {
		name = resolved
	}

	lowered := strings.ToLower(name)

	if info, ok := resolveIntrinsicField(name); ok {
		switch info.kind {
		case intrinsicFieldCreatedAt:
			return compiledSortField{kind: sortFieldCreatedAt, direction: direction, name: info.canonical}, nil
		case intrinsicFieldCreatedBy:
			return compiledSortField{kind: sortFieldCreatedBy, direction: direction, name: info.canonical}, nil
		case intrinsicFieldId:
			return compiledSortField{kind: sortFieldId, direction: direction, name: info.canonical}, nil
		case intrinsicFieldConcept:
			return compiledSortField{kind: sortFieldConcept, direction: direction, name: info.canonical}, nil
		case intrinsicFieldType:
			return compiledSortField{kind: sortFieldType, direction: direction, name: info.canonical}, nil
		default:
			// fallthrough to payload handling
		}
	}

	// Bare payload access (epic #2292): a sort key names a payload property
	// by BARE name (the concept is bound by the query signature), so a
	// non-intrinsic field sorts on the payload surface. The legacy explicit
	// `payload.<path>` form is still accepted (passthrough) for
	// runtime/SDK callers.
	pathExpr := strings.TrimSpace(name)
	if strings.HasPrefix(lowered, "payload.") {
		parts := strings.SplitN(name, ".", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			return compiledSortField{}, fmt.Errorf("payload sort field must include a property path (e.g., \"title\")")
		}
		pathExpr = strings.TrimSpace(parts[1])
	}
	if pathExpr == "" {
		return compiledSortField{}, fmt.Errorf("sort field is required; name a payload property (e.g. \"title\") or an intrinsic")
	}
	path, err := splitRelationshipField(pathExpr)
	if err != nil {
		return compiledSortField{}, fmt.Errorf("sort path %q is invalid: %w", name, err)
	}
	return compiledSortField{
		kind:        sortFieldPayload,
		direction:   direction,
		payloadPath: path,
		name:        "payload." + strings.ToLower(strings.Join(path, ".")),
	}, nil
}

func normalizeSortDirection(direction SortDirection) SortDirection {
	switch strings.ToLower(strings.TrimSpace(string(direction))) {
	case "asc", "ascending":
		return SortDirectionAsc
	default:
		return SortDirectionDesc
	}
}

func (c *compiledSort) signatureValue() string {
	if c == nil || c.signature == "" {
		return "createdAt:desc"
	}
	return c.signature
}

func (c *compiledSort) needsFullScan() bool {
	return c != nil && c.requiresFullScan
}

func (c *compiledSort) orderExpressions() []string {
	if c == nil {
		return []string{`"createdAt" DESC`}
	}
	exprs := make([]string, 0, len(c.fields))
	for _, field := range c.fields {
		exprs = append(exprs, field.sqlOrderExpr())
	}
	return exprs
}

func (f compiledSortField) sqlOrderExpr() string {
	direction := "ASC"
	if f.direction == SortDirectionDesc {
		direction = "DESC"
	}

	switch f.kind {
	case sortFieldCreatedAt:
		return `"createdAt" ` + direction
	case sortFieldCreatedBy:
		return `"createdBy" ` + direction
	case sortFieldId:
		return "id " + direction
	case sortFieldConcept:
		return "concept " + direction
	case sortFieldType:
		return "type " + direction
	case sortFieldPayload:
		expr := fmt.Sprintf("payload #>> '{%s}'", strings.Join(f.payloadPath, ","))
		return fmt.Sprintf("(%s) %s", expr, direction)
	default:
		return `"createdAt" DESC`
	}
}

func (c *compiledSort) sortNodes(nodes []memorynodes.MemoryNode) error {
	if c == nil || len(nodes) <= 1 {
		return nil
	}

	// Pre-compute each node's sort key and pair it WITH the node, then sort the
	// pairs. Sorting the nodes slice alone against a separate keys slice is a
	// correctness bug: sort.SliceStable permutes only the slice it is given
	// (nodes), so a parallel keys[] goes stale on the first swap and the
	// comparator starts reading the wrong key -> scrambled order. Pairing keeps
	// key and node moving together. (This ordering is load-bearing for keyset
	// cursor pagination, which assumes rows arrive in the declared order.)
	type keyedNode struct {
		key  []sortComparable
		node memorynodes.MemoryNode
	}
	keyed := make([]keyedNode, len(nodes))
	for i := range nodes {
		key, err := c.buildKey(nodes[i])
		if err != nil {
			return err
		}
		keyed[i] = keyedNode{key: key, node: nodes[i]}
	}

	// Stable sort so secondary keys remain predictable.
	sort.SliceStable(keyed, func(i, j int) bool {
		return compareSortKeys(keyed[i].key, keyed[j].key) < 0
	})

	for i := range keyed {
		nodes[i] = keyed[i].node
	}
	return nil
}

func (c *compiledSort) buildKey(node memorynodes.MemoryNode) ([]sortComparable, error) {
	key := make([]sortComparable, len(c.fields))
	for idx, field := range c.fields {
		comp, err := field.extractComparable(node)
		if err != nil {
			return nil, err
		}
		key[idx] = comp
	}
	return key, nil
}

func (f compiledSortField) extractComparable(node memorynodes.MemoryNode) (sortComparable, error) {
	comp := sortComparable{direction: f.direction}

	switch f.kind {
	case sortFieldCreatedAt:
		comp.present = true
		comp.kind = sortValueTime
		comp.timeVal = node.CreatedAt
	case sortFieldCreatedBy:
		value := strings.TrimSpace(node.CreatedBy)
		if value != "" {
			comp.present = true
			comp.kind = sortValueString
			comp.str = value
		}
	case sortFieldId:
		value := strings.TrimSpace(node.ID)
		if value != "" {
			comp.present = true
			comp.kind = sortValueString
			comp.str = value
		}
	case sortFieldConcept:
		value := strings.TrimSpace(node.Concept)
		if value != "" {
			comp.present = true
			comp.kind = sortValueString
			comp.str = value
		}
	case sortFieldType:
		value := strings.TrimSpace(node.Type)
		if value != "" {
			comp.present = true
			comp.kind = sortValueString
			comp.str = value
		}
	case sortFieldPayload:
		if len(node.Payload) == 0 {
			return comp, nil
		}
		var payload map[string]any
		if err := json.Unmarshal(node.Payload, &payload); err != nil {
			return comp, fmt.Errorf("parse payload for node %q: %w", node.ID, err)
		}
		value, ok := valueAtPath(payload, f.payloadPath)
		if !ok || value == nil {
			return comp, nil
		}
		switch v := value.(type) {
		case string:
			trimmed := strings.TrimSpace(v)
			if trimmed == "" {
				return comp, nil
			}
			comp.present = true
			comp.kind = sortValueString
			comp.str = v
		case float64:
			comp.present = true
			comp.kind = sortValueNumber
			comp.num = v
		case bool:
			comp.present = true
			comp.kind = sortValueBool
			comp.boolVal = v
		default:
			comp.present = true
			comp.kind = sortValueString
			comp.str = fmt.Sprintf("%v", v)
		}
	}

	return comp, nil
}

func compareSortKeys(a, b []sortComparable) int {
	for idx := range a {
		cmp := compareComparable(a[idx], b[idx])
		if cmp != 0 {
			return cmp
		}
	}
	return 0
}

func compareComparable(a, b sortComparable) int {
	if a.present && !b.present {
		return -1
	}
	if !a.present && b.present {
		return 1
	}
	if !a.present && !b.present {
		return 0
	}

	var raw int
	switch a.kind {
	case sortValueTime:
		if a.timeVal.Before(b.timeVal) {
			raw = -1
		} else if a.timeVal.After(b.timeVal) {
			raw = 1
		} else {
			raw = 0
		}
	case sortValueNumber:
		if a.num < b.num {
			raw = -1
		} else if a.num > b.num {
			raw = 1
		} else {
			raw = 0
		}
	case sortValueBool:
		if a.boolVal == b.boolVal {
			raw = 0
		} else if !a.boolVal && b.boolVal {
			raw = -1
		} else {
			raw = 1
		}
	default:
		raw = strings.Compare(a.str, b.str)
	}

	if a.direction == SortDirectionDesc {
		raw = -raw
	}
	return raw
}

// rowIntrinsicSortField resolves a `row.`-namespaced sort key to the bare
// intrinsic name compileSortField already understands, mirroring
// filterFieldRef for the ordering surface (memql#2779).
//
// Returns "" when name is not `row.`-namespaced, so the caller falls through
// to its existing intrinsic / payload handling untouched. A non-intrinsic
// leaf is an error rather than a fall-through to the payload surface -- that
// fall-through is precisely the silent no-op this closes.
//
// `row.provenance` is rejected with the same message as any other
// non-sortable leaf: provenance is an object-valued intrinsic with no
// ordering, and compileSortField has no branch for it.
func rowIntrinsicSortField(name string) (string, error) {
	head, leaf, found := strings.Cut(strings.TrimSpace(name), ".")
	if !found || !strings.EqualFold(strings.TrimSpace(head), rowIntrinsicNamespace) {
		return "", nil
	}

	leaf = strings.TrimSpace(leaf)
	info, ok := resolveIntrinsicField(leaf)
	if !ok || info.kind == intrinsicFieldProvenance {
		return "", fmt.Errorf(
			"%w: sort key `row.%s` is not a sortable row intrinsic (valid: id, concept, type, createdAt, createdBy) -- a payload property is named BARE in a sort key, so write \"%s\"",
			ErrInvalidArgument, leaf, leaf)
	}
	return info.canonical, nil
}
