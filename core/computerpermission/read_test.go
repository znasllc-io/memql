package computerpermission

import (
	"math"
	"testing"
)

func TestReadContractRejectsAmbiguousOrUnboundedRequests(t *testing.T) {
	for _, args := range []map[string]any{
		{"startLine": 2}, {"startLine": 1, "lineCount": 1, "offsetBytes": 0}, {"search": "x", "startLine": 1, "lineCount": 2},
		{"search": ""}, {"search": "x", "maxMatches": 101}, {"maxBytes": 0}, {"maxBytes": 1.1}, {"maxBytes": math.Inf(1)},
		{"maxScanBytes": 17 * 1024 * 1024, "search": "x"}, {"offset": 0}, {"offsetBytes": -1}, {"maxBytes": true},
	} {
		if _, err := ParseReadOptions(args); err == nil {
			t.Fatalf("accepted invalid read: %v", args)
		}
	}
	for _, args := range []map[string]any{{"path": "/x"}, {"offsetBytes": 10, "maxBytes": 100}, {"startLine": 100, "lineCount": 5}, {"search": "needle", "maxScanBytes": 1024}} {
		if _, err := ParseReadOptions(args); err != nil {
			t.Fatal(err)
		}
	}
}
