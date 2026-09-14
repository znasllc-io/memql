package worker

import (
	"reflect"
	"testing"

	"google.golang.org/grpc"
)

// TestConnectionHandsOutNoRawStream pins the property the send lock
// depends on: the ONLY way to write the worker stream is Connection.Send,
// so there is no path around sendMu. It fails on any exported method
// that returns something implementing grpc.ClientStream -- which is what
// the retired Stream() accessor did -- and on any exported field of that
// shape.
func TestConnectionHandsOutNoRawStream(t *testing.T) {
	clientStream := reflect.TypeOf((*grpc.ClientStream)(nil)).Elem()
	typ := reflect.TypeOf((*Connection)(nil))

	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		for j := 0; j < m.Type.NumOut(); j++ {
			if out := m.Type.Out(j); out.Implements(clientStream) {
				t.Errorf("Connection.%s returns %s, which implements grpc.ClientStream: a caller holding it can SendMsg or CloseSend around sendMu. Remove the accessor; writes go through Connection.Send.", m.Name, out)
			}
		}
	}
	st := typ.Elem()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.IsExported() && f.Type.Implements(clientStream) {
			t.Errorf("Connection.%s is an exported %s: unexport it; writes go through Connection.Send.", f.Name, f.Type)
		}
	}
}
