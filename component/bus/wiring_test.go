package bus

import (
	"context"
	"testing"
	"time"

	busv1 "github.com/visionarys-io/memql/component/bus/gen"
)

func TestNewWiring(t *testing.T) {
	cfg := DefaultChannelConfig()
	w := NewWiring(cfg)

	if w.DbRequests == nil {
		t.Error("expected DbRequests channel")
	}
	if w.EngineRequests == nil {
		t.Error("expected EngineRequests channel")
	}
	if w.IntegrationRequests == nil {
		t.Error("expected IntegrationRequests channel")
	}
	if w.EventPublishCh == nil {
		t.Error("expected EventPublishCh channel")
	}
	if w.ConfigCh == nil {
		t.Error("expected ConfigCh channel")
	}
	if w.TelemetryCh == nil {
		t.Error("expected TelemetryCh channel")
	}
	if w.ReadyCh == nil {
		t.Error("expected ReadyCh channel")
	}
	if w.ShutdownCh == nil {
		t.Error("expected ShutdownCh channel")
	}
}

func TestWiringChannelCapacities(t *testing.T) {
	cfg := ChannelConfig{
		Default: 32,
		Overrides: map[string]int{
			ChanDbRequests: 128,
		},
	}
	w := NewWiring(cfg)

	if w.DbRequests.Cap() != 128 {
		t.Errorf("expected DbRequests cap=128, got %d", w.DbRequests.Cap())
	}
	if w.EngineRequests.Cap() != 32 {
		t.Errorf("expected EngineRequests cap=32, got %d", w.EngineRequests.Cap())
	}
}

func TestWiringChannelsSnapshot(t *testing.T) {
	cfg := DefaultChannelConfig()
	w := NewWiring(cfg)

	infos := w.Channels()
	if len(infos) != 6 {
		t.Errorf("expected 6 channel infos, got %d", len(infos))
	}

	names := make(map[string]bool)
	for _, info := range infos {
		names[info.Name] = true
	}

	expected := []string{
		ChanDbRequests, ChanEngineRequests, ChanIntegrationRequests,
		ChanEventPublish, ChanConfig, ChanTelemetry,
	}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("expected channel %q in snapshot", name)
		}
	}
}

func TestWiringEndToEndRequestResponse(t *testing.T) {
	cfg := DefaultChannelConfig()
	w := NewWiring(cfg)

	// Simulate a database handler
	go func() {
		req := <-w.DbRequests.C
		dbQuery := req.Msg.GetDbQuery()
		if dbQuery == nil {
			return
		}

		resp := NewCorrelatedMessage(req.Msg)
		resp.Payload = &busv1.InternalMessage_DbQueryResponse{
			DbQueryResponse: &busv1.DbQueryResponse{
				Success: true,
				TookMs:  5,
			},
		}
		req.Reply(resp)
	}()

	// Send a request
	msg := NewMessage()
	msg.Payload = &busv1.InternalMessage_DbQuery{
		DbQuery: &busv1.DbQueryRequest{
			Query: "SELECT 1",
		},
	}
	req := NewRequest(msg)

	ctx := context.Background()
	if err := w.DbRequests.SendBlocking(ctx, req); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	resp, err := req.Await(ctx, 1*time.Second)
	if err != nil {
		t.Fatalf("await failed: %v", err)
	}

	dbResp := resp.GetDbQueryResponse()
	if dbResp == nil {
		t.Fatal("expected DbQueryResponse")
	}
	if !dbResp.Success {
		t.Error("expected success=true")
	}
}
