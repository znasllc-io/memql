package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// mapBlobStore is the object store the transports write to when a tarball is
// over the inline cap, as a map. Shared between the two sides of a mesh the way
// one Azure container is shared between the bff and the workbench.
type mapBlobStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMapBlobStore() *mapBlobStore { return &mapBlobStore{objects: map[string][]byte{}} }

func (s *mapBlobStore) Upload(_ context.Context, container, object string, data []byte, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[container+"/"+object] = append([]byte(nil), data...)
	return "https://blob.test/" + container + "/" + object, nil
}

func (s *mapBlobStore) Download(_ context.Context, container, object string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[container+"/"+object]
	if !ok {
		return nil, fmt.Errorf("no object %s/%s", container, object)
	}
	return append([]byte(nil), data...), nil
}

// capturingMesh is buildMesh plus the wire requests it carried, so a test can
// read what the originating side actually put on the mesh.
type capturingMesh struct {
	*buildMesh
	wire []BuildRequest
}

func (m *capturingMesh) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string) (*nodev1.WorkbenchForwardResponse, string, error) {
	var decoded BuildRequest
	if err := json.Unmarshal(req.GetArgsJson(), &decoded); err != nil {
		m.t.Fatalf("the forwarded args are not a build request: %v", err)
	}
	m.wire = append(m.wire, decoded)
	return m.buildMesh.Forward(ctx, req, pinnedNodeId)
}

// TestAnOversizedSourceTravelsByBlobReference pins the transport for a source
// tarball past the inline cap: the originating side stages it in object
// storage and forwards a reference, the workbench reads the reference, and a
// built output past the same cap comes back the same way and is inlined again
// before the caller sees it. Before this, the packages pack always forwarded
// the source inline and a 38 MB storefront tree became a 51 MB message on a
// mesh whose cap is 32 MiB: the stream died with ResourceExhausted, the
// request never arrived, and the run sat at "building" until the hop timed
// out fifteen minutes later.
func TestAnOversizedSourceTravelsByBlobReference(t *testing.T) {
	store := newMapBlobStore()

	workbenchSide, _ := buildIntegration(t)
	workbenchSide.SetBuildBlobStore(store, "memql")
	mesh := &capturingMesh{buildMesh: newBuildMesh(t, workbenchSide)}

	origin := NewIntegration(buildLogger())
	origin.remote = true
	origin.SetBuildForwarder(mesh)
	origin.SetBuildBlobStore(store, "memql")

	req := buildRequest(t, "mkdir -p dist && printf 'hello by reference' > dist/index.html", webFixture())
	// A cap smaller than any real tarball, so both the source and the output
	// are "oversized" without the test carrying megabytes.
	req.Limits.MaxInlineBytes = 64

	res := origin.RunBuild(context.Background(), req, "")
	if !res.OK {
		t.Fatalf("the build must succeed by reference: %s: %s\n%s", res.ErrorCode, res.ErrorMessage, res.LogTail)
	}
	if len(mesh.wire) != 1 {
		t.Fatalf("exactly one forward expected, got %d", len(mesh.wire))
	}
	sent := mesh.wire[0].Source
	if len(sent.Inline) != 0 {
		t.Fatalf("an oversized source must not ride inline; %d bytes were put on the wire", len(sent.Inline))
	}
	if !strings.HasPrefix(sent.Ref, "blob://") {
		t.Fatalf("the forwarded source must be a blob reference, got %q", sent.Ref)
	}
	if got := unpack(t, res.Output.Inline)["dist/index.html"]; got != "hello by reference" {
		t.Fatalf("the caller must see the built bytes inline whatever way they travelled, got %q", got)
	}
}

// TestAnOversizedSourceWithNoObjectStoreIsRefusedBeforeItIsSent pins the
// other half: with no object storage configured, the originating side refuses
// with a typed code that names the cap and the knob, and puts NOTHING on the
// mesh -- because the alternative is the oversized message that kills the
// stream and leaves the run hanging.
func TestAnOversizedSourceWithNoObjectStoreIsRefusedBeforeItIsSent(t *testing.T) {
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "")

	workbenchSide, _ := buildIntegration(t)
	mesh := &capturingMesh{buildMesh: newBuildMesh(t, workbenchSide)}

	origin := NewIntegration(buildLogger())
	origin.remote = true
	origin.SetBuildForwarder(mesh)

	req := buildRequest(t, "mkdir -p dist && touch dist/index.html", webFixture())
	req.Limits.MaxInlineBytes = 64

	res := origin.RunBuild(context.Background(), req, "")
	if res.OK {
		t.Fatal("an oversized source with nowhere to stage it must refuse")
	}
	if res.ErrorCode != BuildCodeSourceTooLarge {
		t.Fatalf("want %s, got %s: %s", BuildCodeSourceTooLarge, res.ErrorCode, res.ErrorMessage)
	}
	if len(mesh.wire) != 0 {
		t.Fatalf("nothing may be forwarded when the source cannot travel; %d request(s) went out", len(mesh.wire))
	}
	for _, want := range []string{"MEMQL_AZURE_BLOB_CONTAINER", "64"} {
		if !strings.Contains(res.ErrorMessage, want) {
			t.Errorf("the refusal must name %s: %q", want, res.ErrorMessage)
		}
	}
}
