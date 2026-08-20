package modules

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// mockStream mirrors sdk/go/pack's scripted-server mock.
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

func (m *mockStream) Send(msg *memqlv1.MemqlClientMessage) error { m.sendCh <- msg; return nil }
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

func reply(t *testing.T, stream *mockStream, fn func(*memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage) {
	t.Helper()
	go func() {
		sent := <-stream.sendCh
		resp := fn(sent)
		resp.CorrelateTo = sent.GetMessageId()
		stream.recvCh <- resp
	}()
}

func newTestClient(t *testing.T) (*Client, *mockStream, func()) {
	t.Helper()
	stream := newMockStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	return NewClient(d), stream, d.Stop
}

func TestList(t *testing.T) {
	c, stream, stop := newTestClient(t)
	defer stop()

	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		if req.GetModulesList() == nil {
			t.Errorf("expected ModulesListMsg, got %T", req.GetPayload())
		}
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ModulesListResult{
				ModulesListResult: &memqlv1.ModulesListResult{
					ReportingNodeId:   "bff-abc",
					ReportingNodeType: "bff",
					Modules: []*memqlv1.ModuleInfo{
						{Kind: "pack", Name: "harness", State: "enabled", Scope: "cluster",
							FqnPrefixes: []string{"integration.harnessRecall."}},
						{Kind: "integration", Name: "email", State: "active", Scope: "node"},
					},
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	inv, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if inv.ReportingNodeID != "bff-abc" || inv.ReportingNodeType != "bff" {
		t.Errorf("reporting node mismatch: %+v", inv)
	}
	if len(inv.Modules) != 2 || inv.Modules[0].Name != "harness" || inv.Modules[0].FqnPrefixes[0] != "integration.harnessRecall." {
		t.Errorf("modules mismatch: %+v", inv.Modules)
	}
}

func TestListRefusalSurfacesAsError(t *testing.T) {
	c, stream, stop := newTestClient(t)
	defer stop()

	reply(t, stream, func(*memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ModulesListResult{
				ModulesListResult: &memqlv1.ModulesListResult{
					ErrorCode:    7,
					ErrorMessage: "module inventory: reading modules requires the owner or admin role",
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.List(ctx)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Code != 7 {
		t.Fatalf("expected RefusedError code 7, got %v", err)
	}
}

func TestGetCarriesEnvSurface(t *testing.T) {
	c, stream, stop := newTestClient(t)
	defer stop()

	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		d := req.GetModuleDetail()
		if d.GetKind() != "component" || d.GetName() != "ai" {
			t.Errorf("detail request mismatch: %+v", d)
		}
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_ModuleDetailResult{
				ModuleDetailResult: &memqlv1.ModuleDetailResult{
					Module: &memqlv1.ModuleInfo{Kind: "component", Name: "ai", State: "built_in"},
					EnvVars: []*memqlv1.ModuleEnvVar{
						{Name: "MEMQL_OPENAI_API_KEY", Secret: true, Set: true},
						{Name: "MEMQL_OBSERVE_LEVEL", Secret: false, Set: true, Value: "count"},
					},
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	detail, err := c.Get(ctx, "component", "ai")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(detail.EnvVars) != 2 {
		t.Fatalf("env vars: %+v", detail.EnvVars)
	}
	if secret := detail.EnvVars[0]; !secret.Secret || secret.Value != "" {
		t.Errorf("secret env var must carry set/unset and no value: %+v", secret)
	}
	if v := detail.EnvVars[1]; v.Value != "count" {
		t.Errorf("variable value lost: %+v", v)
	}
}

func TestSetPackEnabled(t *testing.T) {
	c, stream, stop := newTestClient(t)
	defer stop()

	reply(t, stream, func(req *memqlv1.MemqlClientMessage) *memqlv1.MemqlServerMessage {
		m := req.GetSetPackEnabled()
		if m.GetPackDomain() != "harness" || m.GetEnabled() {
			t.Errorf("flip request mismatch: %+v", m)
		}
		return &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SetPackEnabledResult{
				SetPackEnabledResult: &memqlv1.SetPackEnabledResult{
					PackDomain: "harness", PriorEnabled: true, Enabled: false, RestartRequired: true,
				},
			},
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	flip, err := c.SetPackEnabled(ctx, "harness", false, "maintenance")
	if err != nil {
		t.Fatalf("SetPackEnabled: %v", err)
	}
	if !flip.PriorEnabled || flip.Enabled || !flip.RestartRequired {
		t.Errorf("flip mismatch: %+v", flip)
	}
}
