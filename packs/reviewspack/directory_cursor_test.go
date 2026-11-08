package reviewspack

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestReviewDirectorySearchAndStableContinuation(t *testing.T) {
	rows := []map[string]any{}
	for n := 0; n < 25; n++ {
		row := listRow(fmt.Sprintf("review-%02d", n), n)
		row["title"] = fmt.Sprintf("Recent %02d", n)
		if n >= 13 {
			row["title"] = fmt.Sprintf("Older NEEDLE%%_ %02d", n)
		}
		rows = append(rows, row)
	}
	reader := &listReader{reviews: rows}
	args := map[string]any{"storeId": "store-live", "siteId": "site-1", "limit": 10}
	first := list(t, reader, args)
	if first.Count != 10 || first.Total != 25 || first.Next == "" {
		t.Fatal("wrong first page", first)
	}
	// An insertion before the old waiting frontier must not repeat the tenth row.
	reader.reviews = append(reader.reviews, listRow("arrived-before-frontier", -1))
	args["after"] = first.Next
	second := list(t, reader, args)
	if second.Count != 10 || ids(second)[0] != "review-10" {
		t.Fatal("unstable keyset", ids(second))
	}
	args["after"] = second.Next
	third := list(t, reader, args)
	if third.Count != 5 || third.Next != "" {
		t.Fatal("wrong last page", third)
	}
	searchArgs := map[string]any{"storeId": "store-live", "siteId": "site-1", "limit": 10, "search": "needle%_"}
	matched := list(t, reader, searchArgs)
	if matched.Count != 10 || matched.Total != 12 {
		t.Fatal("search ran after pagination", matched)
	}
	searchArgs["after"] = matched.Next
	last := list(t, reader, searchArgs)
	if last.Count != 2 || last.Next != "" {
		t.Fatal(last)
	}
	for _, changed := range []map[string]any{{"search": "other"}, {"state": "published"}} {
		bad := map[string]any{"storeId": "store-live", "siteId": "site-1", "limit": 10, "search": "needle%_", "after": matched.Next}
		for key, value := range changed {
			bad[key] = value
		}
		if _, err := (&Provider{reader: reader}).list(context.Background(), bad, 0); err == nil || !strings.Contains(err.Error(), "cursor") {
			t.Fatal("accepted a cursor for another directory", err)
		}
	}
}
