package client

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// Both SDKs consume these fixtures: identical wire replies must produce
// identical flattened rows, including the engine's PreserveOrder stamps.
func TestResult_Rows_BuiltinReplyParity(t *testing.T) {
	data, err := os.ReadFile("../../ts/test/fixtures/builtinReply.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name   string         `json:"name"`
		Result map[string]any `json:"result"`
		Rows   []Row          `json:"rows"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			// The data envelope is the actual builtin wire shape. The raw
			// slice and other list envelopes are also accepted by Go Rows.
			for _, key := range []string{"data", "rows", "results", "items", "raw slice"} {
				t.Run(key, func(t *testing.T) {
					items := fixture.Result["data"]
					var payload any = map[string]any{key: items}
					if key == "raw slice" {
						payload = items
					}
					r := &Result{payload: payload}
					before, err := json.Marshal(r.Raw())
					if err != nil {
						t.Fatal(err)
					}
					if got := r.Rows(); !reflect.DeepEqual(got, fixture.Rows) {
						t.Fatalf("Rows() = %#v, want %#v", got, fixture.Rows)
					}
					if got := r.Single(); !reflect.DeepEqual(got, fixture.Rows[0]) {
						t.Fatalf("Single() = %#v, want %#v", got, fixture.Rows[0])
					}
					after, err := json.Marshal(r.Raw())
					if err != nil {
						t.Fatal(err)
					}
					if string(before) != string(after) {
						t.Fatal("Rows() or Single() mutated the wire reply")
					}
				})
			}
		})
	}
}
