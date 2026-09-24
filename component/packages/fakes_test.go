package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// recordingEngine captures every statement and answers reads from a canned
// table. It parses nothing, which is exactly why render_parse_test.go exists.
type recordingEngine struct {
	mu      sync.Mutex
	queries []string
	// rows answers a read whose statement CONTAINS the key. Keyed on a
	// substring rather than parsed, because this fake is deliberately not a
	// second implementation of the front end.
	rows map[string][]map[string]any
	// fail, when set, makes any statement containing the key return an error.
	fail map[string]error
	// Denied by default; lifecycle ordering tests opt in explicitly.
	allowOrganizationActions bool
}

func (e *recordingEngine) OrganizationCapable(context.Context, string, string, string) bool {
	return e.allowOrganizationActions
}

func (e *recordingEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	e.mu.Lock()
	e.queries = append(e.queries, query)
	e.mu.Unlock()

	for key, err := range e.fail {
		if err != nil && contains(query, key) {
			return nil, err
		}
	}
	for key, rows := range e.rows {
		if contains(query, key) {
			if strings.HasPrefix(query, "builtin ") {
				return memql.NewResultWithOutput(asBuiltinNodeSet(rows)), nil
			}
			return &memql.ExecuteResult{Bundle: asBundle(rows)}, nil
		}
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// asBundle builds the wire shape the engine hands back for a bundle-backed
// read. The SHAPED-query branch (rows under `output`) is covered directly in
// rows_test.go rather than through this fake.
func asBundle(rows []map[string]any) *memqlv1.GraphBundle {
	bundle := &memqlv1.GraphBundle{}
	for _, r := range rows {
		payload := map[string]any{}
		id, concept := "", ""
		for k, v := range r {
			switch k {
			case "id":
				id, _ = v.(string)
			case "concept":
				concept, _ = v.(string)
			default:
				payload[k] = v
			}
		}
		st, err := structpb.NewStruct(payload)
		if err != nil {
			st = nil
		}
		bundle.Nodes = append(bundle.Nodes, &memqlv1.MemoryNode{Id: id, Concept: concept, Payload: st})
	}
	return bundle
}

// asBuiltinNodeSet builds what a TOP-LEVEL BUILTIN really hands back: no
// bundle, and an output that is the builtin branch's node set, a
// map[string]MemoryNode keyed by id (engine.go, nodesToMap). Answering a
// builtin with a bundle, as this fake used to, is what hid a reader that
// could not read the real thing: releaseDomainsForSite reported 0 released
// whatever the builtin said.
func asBuiltinNodeSet(rows []map[string]any) map[string]memorynodes.MemoryNode {
	out := make(map[string]memorynodes.MemoryNode, len(rows))
	for n, r := range rows {
		id, _ := r["id"].(string)
		if id == "" {
			id = fmt.Sprintf("builtin:result:%d", n)
		}
		concept, _ := r["concept"].(string)
		raw, err := json.Marshal(r)
		if err != nil {
			panic(fmt.Sprintf("recordingEngine: builtin row not representable: %v (%v)", err, r))
		}
		out[id] = memorynodes.MemoryNode{ID: id, Concept: concept, Payload: raw}
	}
	return out
}

func (e *recordingEngine) statements() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.queries))
	copy(out, e.queries)
	return out
}

// sawStatement reports whether any captured statement contains sub.
func (e *recordingEngine) sawStatement(sub string) bool {
	for _, q := range e.statements() {
		if contains(q, sub) {
			return true
		}
	}
	return false
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
