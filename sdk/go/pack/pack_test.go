package pack

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// mockStream implements memqlv1.MemqlService_StreamClient for testing the
// SDK methods against a scripted server. Mirrors the dispatcher_test mock.
type mockStream struct {
	sendCh chan *memqlv1.MemqlClientMessage
	recvCh chan *memqlv1.MemqlServerMessage
}

func newMockStream() *mockStream {
	return &mockStream{
		sendCh: make(chan *memqlv1.MemqlClientMessage, 10),
		recvCh: make(chan *memqlv1.MemqlServerMessage, 10),
	}
}

func (m *mockStream) Send(msg *memqlv1.MemqlClientMessage) error {
	m.sendCh <- msg
	return nil
}

func (m *mockStream) Recv() (*memqlv1.MemqlServerMessage, error) {
	msg, ok := <-m.recvCh
	if !ok {
		return nil, context.Canceled
	}
	return msg, nil
}

func (m *mockStream) Header() (metadata.MD, error) { return nil, nil }
func (m *mockStream) Trailer() metadata.MD         { return nil }
func (m *mockStream) CloseSend() error             { close(m.recvCh); return nil }
func (m *mockStream) Context() context.Context     { return context.Background() }
func (m *mockStream) SendMsg(any) error            { return nil }
func (m *mockStream) RecvMsg(any) error            { return nil }

// reply drains the next client message and injects a correlated server
// reply built from the client's message_id. fn maps the request to the
// reply payload.
func reply(t *testing.T, stream *mockStream, fn func(*memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage) {
	t.Helper()
	go func() {
		sent := <-stream.sendCh
		resp := fn(sent)
		resp.CorrelateTo = sent.GetMessageId()
		stream.recvCh <- resp
	}()
}

func TestListDomains(t *testing.T) {
	stream := newMockStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	c := NewClient(d)

	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		if req.GetListPackDomains() == nil {
			t.Errorf("expected ListPackDomainsMsg, got %T", req.GetPayload())
		}
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ListPackDomainsResult{
				ListPackDomainsResult: &memqlv1.ListPackDomainsResult{
					Domains: []*memqlv1.PackDomain{
						{Name: "cognition", Origin: "embedded", FileCount: 7},
						{Name: "exampleapp", Origin: "pack:exampleapp", FileCount: 3},
					},
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	domains, err := c.ListDomains(ctx)
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(domains))
	}
	if domains[0].Name != "cognition" || domains[0].Origin != "embedded" || domains[0].FileCount != 7 {
		t.Errorf("domain[0] mismatch: %+v", domains[0])
	}
	if domains[1].Origin != "pack:exampleapp" {
		t.Errorf("expected pack origin, got %q", domains[1].Origin)
	}
}

func TestListFiles(t *testing.T) {
	stream := newMockStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	c := NewClient(d)

	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		if got := req.GetListPackFiles().GetDomain(); got != "cognition" {
			t.Errorf("expected domain 'cognition', got %q", got)
		}
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ListPackFilesResult{
				ListPackFilesResult: &memqlv1.ListPackFilesResult{
					Domain: "cognition",
					Files: []*memqlv1.PackFile{
						{Path: "queries.memql", Size: 1200},
						{Path: "prompts/agentReply.tmpl", Size: 800},
					},
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	files, err := c.ListFiles(ctx, "cognition")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0].Path != "queries.memql" || files[0].Size != 1200 {
		t.Errorf("file[0] mismatch: %+v", files[0])
	}
}

func TestReadFile(t *testing.T) {
	stream := newMockStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	c := NewClient(d)

	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		m := req.GetReadPackFile()
		if m.GetDomain() != "cognition" || m.GetPath() != "queries.memql" {
			t.Errorf("unexpected read request: %+v", m)
		}
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ReadPackFileResult{
				ReadPackFileResult: &memqlv1.ReadPackFileResult{
					Domain: "cognition",
					Path:   "queries.memql",
					Source: "query space q { }",
					Origin: "embedded",
					Found:  true,
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fs, err := c.ReadFile(ctx, "cognition", "queries.memql")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !fs.Found {
		t.Fatal("expected Found=true")
	}
	if fs.Source != "query space q { }" || fs.Origin != "embedded" {
		t.Errorf("read result mismatch: %+v", fs)
	}
}

func TestReadFileNotFound(t *testing.T) {
	stream := newMockStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	c := NewClient(d)

	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ReadPackFileResult{
				ReadPackFileResult: &memqlv1.ReadPackFileResult{
					Domain: "cognition",
					Path:   "nope.memql",
					Found:  false,
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fs, err := c.ReadFile(ctx, "cognition", "nope.memql")
	if err != nil {
		t.Fatalf("ReadFile (not found) returned a transport error: %v", err)
	}
	if fs.Found {
		t.Error("expected Found=false for a missing file")
	}
}
