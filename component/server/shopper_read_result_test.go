package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// Execute returns an envelope whose output is deliberately unexported. A
// plain map fake cannot catch accidental JSON serialization of that envelope,
// which yields Bundle/Meta while losing every returned row.
func TestAShopperReadSerializesEngineOutputRows(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        string
		output      any
		wantRows    int
		wantPayload string
		wantQuery   bool
	}{
		{
			name: "builtin hidden reviews", kind: memql.ShopperReadKindBuiltin,
			output: map[string]memorynodes.MemoryNode{"published": {
				ID: "published", Concept: "v1:reviews:published",
				Payload: json.RawMessage(`{"productHandle":"boot","reviews":[],"count":0}`),
			}},
			wantRows: 1, wantPayload: `{"productHandle":"boot","reviews":[],"count":0}`,
		},
		{
			name: "builtin populated public projection", kind: memql.ShopperReadKindBuiltin,
			output: map[string]memorynodes.MemoryNode{"published": {
				ID: "published", Concept: "v1:reviews:published",
				Payload: json.RawMessage(`{"productHandle":"boot","reviews":[{"id":"review1","title":"Comfortable","body":"Fits well","authorName":"Shopper"}],"count":1}`),
			}},
			wantRows: 1, wantPayload: `{"productHandle":"boot","reviews":[{"id":"review1","title":"Comfortable","body":"Fits well","authorName":"Shopper"}],"count":1}`,
		},
		{
			name: "query projected array", kind: memql.ShopperReadKindQuery,
			output:   []any{map[string]any{"id": "review1", "title": "Projected title"}},
			wantRows: 1, wantQuery: true,
		},
		{name: "empty builtin node set", kind: memql.ShopperReadKindBuiltin, output: map[string]memorynodes.MemoryNode{}},
		{name: "empty query array", kind: memql.ShopperReadKindQuery, output: []any{}},
		{name: "nil engine output", kind: memql.ShopperReadKindQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, executor, _ := shopperFixture(t)
			memql.RegisterShopperRead(memql.ShopperRead{
				Pack: "reviews", Name: "result", Construct: "publicProjection", Kind: tc.kind,
				Fields: []memql.ShopperField{{Name: "productHandle", Required: true, MaxLength: 200}},
			})
			result := memql.NewResultWithOutput(tc.output)
			if tc.wantQuery {
				// Queries may retain a raw graph for engine bookkeeping. Only
				// their selected output belongs in this public response.
				result.Bundle = &memqlv1.GraphBundle{RootIds: []string{"private-unprojected-row"}}
			}
			executor.result = result
			req := httptest.NewRequest(http.MethodGet, "/reads/reviews/result?productHandle=boot", nil)
			for key, value := range goodStamp() {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("data must be an array of output rows: %v; body=%s", err, rec.Body.String())
			}
			if body.Data == nil || len(body.Data) != tc.wantRows {
				t.Fatalf("data = %#v, want non-null array of %d rows", body.Data, tc.wantRows)
			}
			if strings.Contains(rec.Body.String(), `"Bundle"`) || strings.Contains(rec.Body.String(), "private-unprojected-row") {
				t.Fatalf("internal engine graph leaked into public response: %s", rec.Body.String())
			}
			if tc.wantPayload != "" {
				var want map[string]any
				if err := json.Unmarshal([]byte(tc.wantPayload), &want); err != nil {
					t.Fatal(err)
				}
				if body.Data[0]["id"] != "published" || !reflect.DeepEqual(body.Data[0]["payload"], want) {
					t.Fatalf("builtin node envelope or its public payload changed: %#v", body.Data)
				}
			}
			if tc.wantQuery && !reflect.DeepEqual(body.Data, []map[string]any{{"id": "review1", "title": "Projected title"}}) {
				t.Fatalf("query projection changed: %#v", body.Data)
			}
			if len(executor.calls) != 1 || !strings.HasPrefix(executor.calls[0], memql.ShopperCallPrefix(tc.kind)+"publicProjection(") || executor.actor[0] != "user-merchant" {
				t.Fatalf("declared call or owner changed: calls=%v actors=%v", executor.calls, executor.actor)
			}
		})
	}
}
