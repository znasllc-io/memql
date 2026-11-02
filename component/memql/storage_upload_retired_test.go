package memql

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"sync"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// storage_upload_retired_test.go -- no builtin hands a caller's object name to
// the storage integration.
//
// `storageUpload` and the `integration.storage.upload` capability behind it are
// retired. Blob storage is written by native producers, each composing its
// object names under its own authority, and nothing called this one.
//
// THE CALLS UNDER TEST ARE THE ONES A CLIENT CAN SEND, through Execute, in every
// spelling: the `builtin` statement, and the bare call the meta-command path
// resolves against the same registry, with named arguments or with one object.
// The parse, the builtin lookup and the dispatch are the production path. Only
// the handler is a fixture, registered under the integration name and
// capability the builtin named, so a declaration that came back naming the
// executor reaches the recorder and fails this file. What the real handler
// would have done with the call is not what this file measures.
//
// THE CONTROL IS A FIXTURE BUILTIN naming the same executor. It reaches the
// recorder for the same caller, so a refusal below is the embedded tree's
// answer and never a harness that could not have dispatched anything.

// storageUploadProbeArgs is what each call below passes.
var storageUploadProbeArgs = map[string]any{
	"bucket":     "probe-container",
	"objectName": "probe/object.json",
	"data":       "{}",
}

// storageUploadCalls are the spellings of the call.
var storageUploadCalls = []string{
	`builtin storageUpload(bucket: "probe-container", objectName: "probe/object.json", data: "{}")`,
	`storageUpload(bucket: "probe-container", objectName: "probe/object.json", data: "{}")`,
	`storageUpload({"bucket": "probe-container", "objectName": "probe/object.json", "data": "{}"})`,
}

// storageUploadControl is the fixture builtin declared in the test, naming the
// executor the retired builtin named.
const storageUploadControl = "storageUploadProbeControl"

// storageHandlerProbe stands in for the storage integration, registered under
// the SAME name and capability so an `@executor("integration.storage.upload")`
// resolves to it. It records every argument map it is handed.
type storageHandlerProbe struct {
	mu      sync.Mutex
	reached []map[string]any
}

func (p *storageHandlerProbe) IntegrationName() string { return "storage" }

func (p *storageHandlerProbe) Capabilities() []IntegrationCapability {
	return []IntegrationCapability{{
		Name:        "upload",
		Description: "records that the handler was reached, and with what",
		Handler: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.reached = append(p.reached, maps.Clone(args))
			return nil, nil
		},
	}}
}

func (p *storageHandlerProbe) calls() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.reached...)
}

// storageUploadProbe builds a DB-free engine holding the embedded tree's
// builtins plus the control, with the storage executor answered by a recorder.
func storageUploadProbe(t *testing.T) (*MemQLEngine, *storageHandlerProbe) {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fns := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(slog.New(slog.NewTextHandler(io.Discard, nil)), fns); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	if err := fns.add(&Function{
		Name:         storageUploadControl,
		Type:         FunctionTypeBuiltin,
		FunctionKind: "builtin",
		Executor:     "integration.storage.upload",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("declare the control builtin: %v", err)
	}
	e := &MemQLEngine{
		initialized:  true,
		functions:    fns,
		integrations: newIntegrationRegistry(),
		db:           neverDialledDB(t),
	}
	probe := &storageHandlerProbe{}
	if err := e.RegisterIntegration(probe); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return e, probe
}

// storageUploadCallers is every person a stream can carry, lowest first, and a
// stream that carries nobody. The owner is on it: retiring a builtin retires it
// for everyone.
var storageUploadCallers = []struct {
	name string
	ctx  func() context.Context
}{
	{"no identity", context.Background},
	{"viewer", func() context.Context { return asCaller("viewer") }},
	{"user", func() context.Context { return asCaller("user") }},
	{"admin", func() context.Context { return asCaller("admin") }},
	{"developer", func() context.Context { return asCaller("developer") }},
	{"owner", func() context.Context { return asCaller("owner") }},
}

// TestNoCallerReachesTheStorageExecutorThroughStorageUpload runs the call for
// every caller in every spelling.
func TestNoCallerReachesTheStorageExecutorThroughStorageUpload(t *testing.T) {
	installSeededCatalog(t)
	e, probe := storageUploadProbe(t)

	for _, caller := range storageUploadCallers {
		for _, call := range storageUploadCalls {
			before := len(probe.calls())
			_, err := e.Execute(caller.ctx(), call)
			if got := probe.calls(); len(got) != before {
				t.Errorf("%s reached the storage executor with %v through\n  %s",
					caller.name, got[len(got)-1], call)
				continue
			}
			if err == nil {
				t.Errorf("%s's call answered success without reaching anything:\n  %s", caller.name, call)
			}
		}
	}
}

// TestTheStorageExecutorIsReachableThroughADeclaration is the control: a
// builtin that names the executor reaches the recorder for the lowest caller,
// with the arguments it was handed. Without it, the test above passes on a
// harness that dispatches nothing.
func TestTheStorageExecutorIsReachableThroughADeclaration(t *testing.T) {
	installSeededCatalog(t)
	e, probe := storageUploadProbe(t)

	call := `builtin ` + storageUploadControl + `(bucket: "probe-container", objectName: "probe/object.json", data: "{}")`
	if _, err := e.Execute(asCaller("viewer"), call); err != nil {
		t.Fatalf("the control builtin did not dispatch for a viewer: %v", err)
	}
	got := probe.calls()
	if len(got) != 1 {
		t.Fatalf("the control reached the recorder %d times, want 1", len(got))
	}
	if !maps.Equal(got[0], storageUploadProbeArgs) {
		t.Fatalf("the control reached the recorder with %v, want %v", got[0], storageUploadProbeArgs)
	}
}
