package accountexport

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"
)

func TestHandOverRegistry(t *testing.T) {
	t.Cleanup(resetHandOversForTest())
	h := func(context.Context, Querier, HandOverInput, HandOverSink) error { return nil }
	RegisterHandOver("zeta", h)
	RegisterHandOver("alpha", h)
	if got := HandOvers(); strings.Join(got, ",") != "alpha,zeta" || !sort.StringsAreSorted(got) {
		t.Fatalf("HandOvers() = %v", got)
	}
	if err := RunHandOver(context.Background(), "nobody", nil, HandOverInput{}, nil); err == nil {
		t.Fatal("an unregistered hand-over ran")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate hand-over name did not panic")
		}
	}()
	RegisterHandOver("alpha", h)
}

// A hand-over writes its own members through the writer: a single member and
// a parted folder under tables/, both counted in the report and checked.
func TestHandOverWritesThroughTheWriter(t *testing.T) {
	t.Cleanup(resetHandOversForTest())
	RegisterHandOver("email-blocks", func(_ context.Context, _ Querier, in HandOverInput, out HandOverSink) error {
		for _, id := range in.RowIDs("v1:email:list") {
			if err := out.Line(MemberBlocks, map[string]any{"list": id, "account": in.Account}); err != nil {
				return err
			}
		}
		return nil
	})
	RegisterHandOver("site-statistics", func(_ context.Context, _ Querier, in HandOverInput, out HandOverSink) error {
		p := out.Parted("site-statistics/hourly")
		for _, id := range in.RowIDs("v1:platform:site") {
			if err := p.Write(map[string]any{"siteId": id, "views": 3}); err != nil {
				return err
			}
		}
		return p.Close()
	})

	c := defaultFixture()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, fixtureStem, fixtureManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range c.accountRows {
		if err := w.AccountRow(row); err != nil {
			t.Fatal(err)
		}
	}
	in := HandOverInput{Account: "acc0001", RowIDs: func(concept string) []string {
		return map[string][]string{"v1:email:list": {"l1", "l2"}, "v1:platform:site": {"s1"}}[concept]
	}}
	for _, name := range HandOvers() {
		if err := RunHandOver(context.Background(), name, nil, in, w.HandOverSink()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	r := c.report()
	r.Kinds = r.Kinds[:1]
	r.Bytes = BytesReport{}
	r.Members = []MemberCount{{Name: MemberBlocks, Lines: 2}, {Name: "tables/site-statistics/hourly", Lines: 1}}
	if err := w.Finish(r); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if _, err := Verify(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got := strings.Join(names(readEntries(t, buf.Bytes())), "\n")
	if !strings.Contains(got, fixtureStem+"/"+MemberBlocks+"\n"+fixtureStem+"/tables/site-statistics/hourly/part-000001.jsonl.gz") {
		t.Fatalf("hand-over members missing or out of order:\n%s", got)
	}

	// A hand-over may not write the run's own members.
	var b2 bytes.Buffer
	w2, _ := NewWriter(&b2, fixtureStem, fixtureManifest())
	if err := w2.HandOverSink().Line(MemberPeople, map[string]any{}); err == nil {
		t.Fatal("a hand-over wrote people.jsonl")
	}
}
