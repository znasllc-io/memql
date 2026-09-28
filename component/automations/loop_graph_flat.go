package automations

// loop_graph_flat.go -- the static loop graph as a graph FILE records it
// (memql#5727).
//
// ONE BUILDER, THREE READERS. LoopGraphRows is the projection of a LoopGraph:
// it decides what a row carries -- the trigger folded back into (kind,
// concept), the file an automation came from, its cycle and its problem, its
// edges in a stable order. The automationGraph builtin serves those rows to the
// OS's Cluster > Automations section. The architecture model (cmd/memql-arch)
// and the platform graph (cmd/platformgraph) record the same graph as nodes
// and edges with string attributes, and FlattenLoopGraph is how: it reads
// LoopGraphRows and never the LoopGraph, so the three cannot describe one
// automation two ways. A fact added to a row reaches every graph file by being
// flattened here, not by a second walk of the graph.

import (
	"fmt"
	"strconv"
	"strings"
)

// FlatAutomation is one automation of the static loop graph with every fact a
// string.
type FlatAutomation struct {
	Name string
	// File is the file the automation was loaded from, relative to the DSL
	// root (cluster/automations.memql), or the loader's origin when it did not
	// come from the tree.
	File string
	// Writes is the concept ids the automation writes, in the graph's order.
	Writes []string
	// Attrs carries stratum and, when present, template, trigger,
	// triggerKind, triggerConcept, schedule, filter, writes, publishes, loop,
	// mode, cycle, cyclePermitted and problem. Absent rather than empty.
	Attrs map[string]string
}

// FlatTrigger is one edge of the static loop graph: From's writes or publishes
// can fire To.
type FlatTrigger struct {
	From, To string
	// Attrs carries concept, topic, via, decided and reason, absent when empty.
	Attrs map[string]string
}

// FlattenLoopGraph flattens LoopGraphRows(g), in the rows' order: automations
// by name, each one's outgoing edges by target, concept and topic.
func FlattenLoopGraph(g *LoopGraph) ([]FlatAutomation, []FlatTrigger) {
	rows := LoopGraphRows(g)
	autos := make([]FlatAutomation, 0, len(rows))
	var triggers []FlatTrigger
	for _, row := range rows {
		name := rowString(row, "name")
		writes := rowStrings(row, "writes")
		attrs := map[string]string{
			"stratum":        rowScalar(row, "stratum"),
			"trigger":        rowString(row, "trigger"),
			"triggerKind":    rowString(row, "triggerKind"),
			"triggerConcept": rowString(row, "triggerConcept"),
			"schedule":       rowString(row, "schedule"),
			"filter":         rowString(row, "triggerFilter"),
			"writes":         strings.Join(writes, ","),
			"publishes":      strings.Join(rowStrings(row, "publishes"), ","),
			"problem":        rowString(row, "problem"),
		}
		if t, _ := row["template"].(bool); t {
			attrs["template"] = "true"
		}
		if loop, ok := row["loop"].(map[string]any); ok {
			attrs["loop"] = fmt.Sprintf("maxDepth=%s until=%s", rowScalar(loop, "maxDepth"), rowString(loop, "until"))
		}
		if mode, ok := row["mode"].(map[string]any); ok {
			attrs["mode"] = rowString(mode, "kind")
			if max := rowScalar(mode, "max"); max != "" {
				attrs["mode"] += " max=" + max
			}
		}
		if cycle, ok := row["cycle"].(map[string]any); ok {
			attrs["cycle"] = strings.Join(rowStrings(cycle, "members"), ",")
			attrs["cyclePermitted"] = rowScalar(cycle, "permitted")
		}
		for k, v := range attrs {
			if v == "" {
				delete(attrs, k)
			}
		}
		autos = append(autos, FlatAutomation{Name: name, File: rowString(row, "origin"), Writes: writes, Attrs: attrs})

		edges, _ := row["edgesOut"].([]map[string]any)
		for _, e := range edges {
			ea := map[string]string{
				"concept": rowString(e, "concept"),
				"topic":   rowString(e, "topic"),
				"via":     strings.Join(rowStrings(e, "via"), ","),
				"decided": rowScalar(e, "decided"),
				"reason":  rowString(e, "reason"),
			}
			for k, v := range ea {
				if v == "" {
					delete(ea, k)
				}
			}
			triggers = append(triggers, FlatTrigger{From: name, To: rowString(e, "to"), Attrs: ea})
		}
	}
	return autos, triggers
}

func rowString(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

// rowScalar renders the ints and bools a row carries; "" when absent.
func rowScalar(row map[string]any, key string) string {
	switch v := row[key].(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// rowStrings reads a row's string list in the row's order.
func rowStrings(row map[string]any, key string) []string {
	switch v := row[key].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
