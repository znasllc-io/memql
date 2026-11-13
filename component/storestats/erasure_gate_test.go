package storestats

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// erasure_gate_test.go -- what the facts read survives a customer's erasure.
//
// A fact is folded from mirror rows, and a privacy erasure rewrites those
// rows. A field the erasure scrubs could not be taken out of a fact again by
// anything but the check, so every field the statistics shapes project from a
// row an erasure reaches must be one the erasure keeps, by its own answer
// (shopify.ErasureDecision). The shapes are read from the DSL itself, so a
// field added to a shape is gated the day it is added.

// shapesFile is the overlay the walk's shapes are declared in.
var shapesFile = filepath.Join("..", "..", "dsl", "shopify", "overlay", "shapes.memql")

// gatedShapes are the four shapes the facts are made from, over the three
// concepts an erasure reaches. The shop and the store rows are outside the
// gate: a customer's erasure reaches neither.
var gatedShapes = []string{"storeStatisticsOrder", "storeStatisticsLine", "storeStatisticsRefund", "storeStatisticsBuyer"}

// keptAnswers are the erasure's answers a gated field may have.
var keptAnswers = map[string]bool{"kept as structure": true, "kept by kind": true, "kept by the list": true}

// admittedByName is the one other answer admitted, for the one field: a custom
// item's title, which the check repairs.
var admittedByName = map[string]string{"orderLineItem.title": "kept, scrubbed on a custom item"}

// exceptedByName is read and never stored: the email decides wholesale.
var exceptedByName = map[string]bool{"order.email": true}

type statShape struct {
	concept string
	fields  []string
}

var shapeHeader = regexp.MustCompile(`^shape\s+(\w+)\s+(\w+)\s*\{\s*$`)

// readShapes parses every shape of the overlay: its concept and its fields.
func readShapes(t *testing.T) map[string]statShape {
	t.Helper()
	f, err := os.Open(shapesFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]statShape{}
	var name string
	var cur statShape
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if name == "" {
			if m := shapeHeader.FindStringSubmatch(line); m != nil {
				name, cur = m[2], statShape{concept: m[1]}
			}
			continue
		}
		if line == "}" {
			out[name] = cur
			name = ""
			continue
		}
		if line != "" {
			cur.fields = append(cur.fields, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Fatalf("shape %s is not closed", name)
	}
	return out
}

// gateFailures is the gate over one shape's fields, one message a field that
// fails: the shape, the field and the erasure's answer.
func gateFailures(shape, concept string, fields []string) []string {
	var out []string
	for _, field := range fields {
		if strings.HasPrefix(field, "row.") || exceptedByName[concept+"."+field] {
			continue
		}
		answer := shopify.ErasureDecision(concept, field)
		if want, ok := admittedByName[concept+"."+field]; ok {
			if answer != want {
				out = append(out, shape+": "+field+": the erasure answers "+answer+", want "+want)
			}
			continue
		}
		if !keptAnswers[answer] {
			out = append(out, shape+": "+field+": the erasure answers "+answer)
		}
	}
	return out
}

func TestEveryFieldTheFactsReadSurvivesAnErasure(t *testing.T) {
	shapes := readShapes(t)
	for _, name := range gatedShapes {
		s, ok := shapes[name]
		if !ok {
			t.Errorf("shape %s is not in %s", name, shapesFile)
			continue
		}
		if len(s.fields) == 0 {
			t.Errorf("shape %s has no field", name)
			continue
		}
		for _, failure := range gateFailures(name, s.concept, s.fields) {
			t.Error(failure)
		}
	}
	// The admitted and excepted fields are fields of the shapes: an entry
	// nothing reads any more is a hole kept open for nothing.
	read := map[string]bool{}
	for _, name := range gatedShapes {
		for _, f := range shapes[name].fields {
			read[shapes[name].concept+"."+f] = true
		}
	}
	for k := range admittedByName {
		if !read[k] {
			t.Errorf("%s is admitted by name and no gated shape reads it", k)
		}
	}
	for k := range exceptedByName {
		if !read[k] {
			t.Errorf("%s is excepted by name and no gated shape reads it", k)
		}
	}
}

func TestTheGateFailsOnAFieldAnErasureScrubs(t *testing.T) {
	order := readShapes(t)["storeStatisticsOrder"]
	if len(order.fields) == 0 {
		t.Fatal("storeStatisticsOrder has no field")
	}
	if got := gateFailures("storeStatisticsOrder", order.concept, order.fields); len(got) != 0 {
		t.Fatalf("the order shape fails before anything is added: %v", got)
	}
	for added, answer := range map[string]string{"note": "scrubbed", "noSuchField": "no such field"} {
		fields := append(append([]string(nil), order.fields...), added)
		got := gateFailures("storeStatisticsOrder", order.concept, fields)
		if len(got) != 1 || !strings.Contains(got[0], added) || !strings.Contains(got[0], answer) || !strings.Contains(got[0], "storeStatisticsOrder") {
			t.Errorf("with %s added: %v; want one failure naming the shape, %s and %q", added, got, added, answer)
		}
	}
	// The admitted answer is admitted for its field only.
	line := readShapes(t)["storeStatisticsLine"]
	if got := gateFailures("storeStatisticsLine", line.concept, append(append([]string(nil), line.fields...), "name")); len(got) != 1 || !strings.Contains(got[0], "name") {
		t.Errorf("a line's name, also scrubbed on a custom item: %v; want it to fail", got)
	}
}

// recording is a row that remembers every key read of it.
type recording struct {
	m    map[string]any
	seen map[string]bool
}

func (r recording) get(key string) any {
	r.seen[key] = true
	return r.m[key]
}

func TestTheFactFunctionsReadNoKeyOutsideTheirShapes(t *testing.T) {
	shapes := readShapes(t)
	allowed := func(shape string) map[string]bool {
		out := map[string]bool{}
		for _, f := range shapes[shape].fields {
			out[strings.TrimPrefix(f, "row.")] = true
		}
		return out
	}
	check := func(shape string, rows []map[string]any, run func(fields)) {
		t.Helper()
		ok := allowed(shape)
		seen := map[string]bool{}
		for _, row := range rows {
			run(recording{m: row, seen: seen})
		}
		if len(seen) == 0 {
			t.Errorf("%s: nothing was read", shape)
		}
		var outside []string
		for k := range seen {
			if !ok[k] {
				outside = append(outside, k)
			}
		}
		sort.Strings(outside)
		if len(outside) > 0 {
			t.Errorf("%s: read %v, which the shape does not project", shape, outside)
		}
	}
	gone := func(row map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range row {
			out[k] = v
		}
		out["deleted"] = true
		return out
	}
	o, l, r := orderRow(), lineRow(), refundRow()
	o["originalTotalDutiesSet"] = set("1.00", "EUR")
	o["cancelledAt"] = "2026-11-02T00:00:00Z"
	unread := refundRow()
	unread["refundLineItems"] = nil
	check("storeStatisticsOrder", []map[string]any{o, gone(o)}, func(f fields) { orderFactFrom(f) })
	check("storeStatisticsLine", []map[string]any{l, gone(l)}, func(f fields) { lineFactFrom(f) })
	check("storeStatisticsRefund", []map[string]any{r, gone(r), unread}, func(f fields) { refundFactFrom(f, 50) })
}

func TestTheWalkedConceptsAreEveryMirroredConceptTheShapesRead(t *testing.T) {
	shapes := readShapes(t)
	read := map[string]bool{}
	n := 0
	for name, s := range shapes {
		if !strings.HasPrefix(name, "storeStatistics") {
			continue
		}
		n++
		if s.concept != "store" {
			read[s.concept] = true
		}
	}
	if n != 7 {
		t.Errorf("%d storeStatistics shapes in %s, want seven including the connection markers", n, shapesFile)
	}
	in := map[string]bool{}
	for _, c := range walkedConcepts {
		in[c] = true
		if generated.Types[c] == nil {
			t.Errorf("walkedConcepts holds %q, which is not a generated concept", c)
		}
		if !read[c] {
			t.Errorf("walkedConcepts holds %q, which no shape reads", c)
		}
	}
	for c := range read {
		if !in[c] {
			t.Errorf("a shape reads %q and walkedConcepts does not hold it", c)
		}
	}
	if len(walkedConcepts) < 3 || walkedConcepts[0] != "order" || walkedConcepts[1] != "orderLineItem" || walkedConcepts[2] != "refund" {
		t.Errorf("walkedConcepts = %v; the first three are the walk's sources, order, orderLineItem and refund", walkedConcepts)
	}
	if in["store"] {
		t.Error("walkedConcepts holds store, the engine's own row")
	}
}
