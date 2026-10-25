package client

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// Exercise the named method and actual protobuf-to-protojson boundary. A
// ResultFromRows fake cannot reproduce omitted repeated data/nodes fields.
func TestGlobalVariableEmptyProtoEnvelopeHasNoRows(t *testing.T) {
	count := int64(0)
	emptyObject := structpb.NewStructValue(&structpb.Struct{})
	variable, err := structpb.NewStruct(map[string]any{"id": "variable-id", "name": "TEST_OPTIONAL_VARIABLE", "value": ""})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		wire *memqlv1.Result
		want []Row
	}{
		{"nil result", nil, nil},
		{"empty result", &memqlv1.Result{}, nil},
		{"metadata only", &memqlv1.Result{Meta: &memqlv1.ResultMeta{Count: &count, ServerId: "replica-b"}}, nil},
		{"empty bundle", &memqlv1.Result{Bundle: &memqlv1.GraphBundle{}}, nil},
		{"empty bundle with metadata", &memqlv1.Result{Bundle: &memqlv1.GraphBundle{}, Meta: &memqlv1.ResultMeta{Count: &count}}, nil},
		{"actual empty object", &memqlv1.Result{Data: []*structpb.Value{emptyObject}}, []Row{{}}},
		{"empty bundle with actual empty object", &memqlv1.Result{Bundle: &memqlv1.GraphBundle{}, Data: []*structpb.Value{emptyObject}}, []Row{{}}},
		{"projected variable", &memqlv1.Result{Data: []*structpb.Value{structpb.NewStructValue(variable)}}, []Row{{"id": "variable-id", "name": "TEST_OPTIONAL_VARIABLE", "value": ""}}},
		{"bundle variable", &memqlv1.Result{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{Id: "variable-id", Concept: "v1:platform:globalVariable", Payload: variable}}}}, []Row{{"id": "variable-id", "concept": "v1:platform:globalVariable", "name": "TEST_OPTIONAL_VARIABLE", "value": ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newMockStream()
			d := NewDispatcher(stream, nil)
			stopped := make(chan struct{})
			go func() { d.Run(); close(stopped) }()
			t.Cleanup(func() {
				d.Stop()
				_ = stream.CloseSend()
				select {
				case <-stopped:
				case <-time.After(2 * time.Second):
					t.Error("owned dispatcher did not stop")
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			type answer struct {
				result *Result
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				r, e := NewQueryClient(d).GlobalVariable(ctx, GlobalVariableArgs{Name: "TEST_OPTIONAL_VARIABLE"})
				done <- answer{r, e}
			}()
			var sent *memqlv1.MemqlClientMessage
			select {
			case sent = <-stream.sendCh:
			case <-ctx.Done():
				t.Fatal("named query was not sent")
			}
			if got := sent.GetExecuteQuery().GetQuery(); got != `query globalVariable(name: "TEST_OPTIONAL_VARIABLE")` {
				t.Fatalf("unexpected named query: %q", got)
			}
			stream.recvCh <- &memqlv1.MemqlServerMessage{CorrelateTo: sent.GetMessageId(), Payload: &memqlv1.MemqlServerMessage_QueryResult{QueryResult: &memqlv1.QueryResultChunk{Result: tc.wire, Done: true}}}
			select {
			case got := <-done:
				if got.err != nil || got.result == nil {
					t.Fatalf("query failed: %v", got.err)
				}
				rows := got.result.Rows()
				if len(rows) != len(tc.want) || len(rows) > 0 && !reflect.DeepEqual(rows, tc.want) {
					t.Fatalf("Rows() = %#v; want %#v", rows, tc.want)
				}
				if len(tc.want) == 0 && got.result.Single() != nil {
					t.Fatal("empty result became a single row")
				}
				if tc.wire.GetMeta() != nil {
					raw, ok := got.result.Raw().(map[string]any)
					if !ok || raw["meta"] == nil {
						t.Fatal("result metadata was dropped")
					}
				}
			case <-ctx.Done():
				t.Fatal("named query did not finish")
			}
		})
	}
}

func TestGlobalVariableProtoErrorIsNotAbsence(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	stopped := make(chan struct{})
	go func() { d.Run(); close(stopped) }()
	t.Cleanup(func() {
		d.Stop()
		_ = stream.CloseSend()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("owned dispatcher did not stop")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewQueryClient(d).GlobalVariable(ctx, GlobalVariableArgs{Name: "TEST_OPTIONAL_VARIABLE"})
		done <- err
	}()
	var sent *memqlv1.MemqlClientMessage
	select {
	case sent = <-stream.sendCh:
	case <-ctx.Done():
		t.Fatal("named query was not sent")
	}
	stream.recvCh <- &memqlv1.MemqlServerMessage{CorrelateTo: sent.GetMessageId(), Payload: &memqlv1.MemqlServerMessage_QueryError{QueryError: &memqlv1.QueryErrorMsg{Error: &memqlv1.QueryError{Message: "access denied"}}}}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Fatalf("query error became absence: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("named query did not finish")
	}
}

func TestResultFlatRowsRemainRows(t *testing.T) {
	for _, row := range []Row{{}, {"id": "one", "value": ""}, {"meta": map[string]any{"count": float64(0)}}} {
		r := &Result{payload: row}
		if got := r.Rows(); len(got) != 1 || !reflect.DeepEqual(got[0], row) {
			t.Fatalf("ordinary flat row changed: %#v", got)
		}
	}
}
