package deploycontrol

import (
	"reflect"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestNoRpcTakesAnEnvironment is the post-epic assertion for the deploy
// surface (epic memql#3943): the console operates on THIS installation, so
// no request it accepts and no status it returns names an environment.
//
// # Why reflection over the generated types rather than a grep
//
// A grep over the .proto catches the field while it is spelled `env`. It does
// not catch `environment`, `target_env`, or an enum wrapper -- and the failure
// this guards against is not a copy of the deleted field, it is a NEW one
// arriving under a name nobody thought to search for. Walking the generated
// structs asks the question the way the wire asks it: does any message on this
// service carry a field whose name is about which environment a call means.
//
// # Why it walks the interface rather than a hand-listed set
//
// The RPC list comes off the generated server interface, so an RPC added later
// is covered without an edit here. That is the same reason
// TestBelowFloorInvalidArgumentCoverage derives its set that way, and the same
// failure it avoids: a guard that is narrower than the surface it guards
// passes for the wrong reason.
//
// This is NOT a substitute for TestNoEnvironmentBranchingInEngineCode, which
// scans engine SOURCE for environment names. That gate is about behaviour that
// can tell two environments apart; this one is about the wire contract still
// offering a caller a way to name one.
func TestNoRpcTakesAnEnvironment(t *testing.T) {
	iface := reflect.TypeOf((*memqlv1.DeployControlServiceServer)(nil)).Elem()
	checked := 0
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		if strings.HasPrefix(m.Name, "mustEmbedUnimplemented") {
			continue
		}
		// (ctx, *Request) -> (*Response, error).
		for _, param := range []reflect.Type{m.Type.In(1), m.Type.Out(0)} {
			msg := param
			for msg.Kind() == reflect.Ptr {
				msg = msg.Elem()
			}
			if msg.Kind() != reflect.Struct {
				continue
			}
			checked++
			for f := 0; f < msg.NumField(); f++ {
				name := strings.ToLower(msg.Field(f).Name)
				if name == "env" || strings.Contains(name, "environment") {
					t.Errorf("%s: %s.%s names a deployment environment -- this surface "+
						"operates on THIS installation, and a second environment is a "+
						"second install with its own address (epic memql#3943)",
						m.Name, msg.Name(), msg.Field(f).Name)
				}
			}
		}
	}
	// A reflection walk that saw nothing would report "no environments" for
	// the same reason an empty list has no violations.
	if checked < 14 {
		t.Fatalf("walked only %d messages across the service; the guard cannot "+
			"pass vacuously", checked)
	}
}

// TestGetDeploymentStatusTakesNoParameter is the narrower half of the claim
// above, stated where a reader looks for it: the read answers for this
// installation and there is nothing to select.
//
// It asserts the REQUEST is empty of its own fields rather than asserting the
// method signature, because the signature keeps its request parameter whatever
// the message holds -- an env field re-added to GetDeploymentStatusRequest
// would leave the signature untouched and this is what would notice.
func TestGetDeploymentStatusTakesNoParameter(t *testing.T) {
	req := reflect.TypeOf(memqlv1.GetDeploymentStatusRequest{})
	for i := 0; i < req.NumField(); i++ {
		f := req.Field(i)
		// protoc-gen-go emits three unexported bookkeeping fields on every
		// message (state / sizeCache / unknownFields); a declared proto field
		// is exported.
		if f.PkgPath != "" {
			continue
		}
		t.Errorf("GetDeploymentStatusRequest.%s: the request is deliberately "+
			"empty -- the console answers for this installation (epic memql#3943)",
			f.Name)
	}
}
