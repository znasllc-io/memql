//go:build agent

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// The caller owns neither the machine stream nor its registration. More than
// the former inline archive limit must cross the real dispatcher/forward hop
// in order, before Dispatch returns its terminal digest. Waiting on the link
// before checking would hide a result that overtook its artifact frames.
func TestPipelineArtifactChunksFinishBeforeResultAcrossReplicas(t *testing.T) {
	const chunks = 100
	const chunkBytes = 256 << 10
	expected := sha256.New()
	h := pipelineHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, send func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		for i := 0; i < chunks; i++ {
			data := make([]byte, chunkBytes)
			for j := range data {
				data[j] = byte(i + j)
			}
			expected.Write(data)
			send(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_DataChunk{DataChunk: data}})
		}
		result := okResult(d.GetCallId())
		result.GetSuccess().ResultJson = []byte(fmt.Sprintf(`{"artifactsArchiveBytes":%d,"artifactsArchiveSHA256":"sha256:%x"}`, chunks*chunkBytes, expected.Sum(nil)))
		return result, nil
	})
	var mu sync.Mutex
	actual := sha256.New()
	count := 0
	req := pipelineRequest()
	req.OwnerUserId = h.owner
	req.OnStreamChunk = func(frame *nodev1.WorkerForwardStream) {
		mu.Lock()
		defer mu.Unlock()
		data := frame.GetDataChunk()
		if len(data) != chunkBytes {
			t.Errorf("bad frame size: %d", len(data))
		}
		actual.Write(data)
		count++
	}
	res, err := h.dispatch.Dispatch(auth.ContextWithInternalOrigin(authorityCtx(t, h.owner)), req)
	if err != nil || !res.OK {
		t.Fatalf("dispatch: %+v %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	var terminal struct {
		Size   int64  `json:"artifactsArchiveBytes"`
		Digest string `json:"artifactsArchiveSHA256"`
	}
	if err := json.Unmarshal([]byte(res.OutputJSON), &terminal); err != nil {
		t.Fatal(err)
	}
	if count != chunks || terminal.Size != chunks*chunkBytes || terminal.Digest != fmt.Sprintf("sha256:%x", actual.Sum(nil)) {
		t.Fatalf("terminal overtook or corrupted stream: count=%d terminal=%+v", count, terminal)
	}
}
