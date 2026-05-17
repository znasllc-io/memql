package bus

import (
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

// Channel name constants used for config overrides and telemetry identification.
const (
	ChanDbRequests          = "db.requests"
	ChanEngineRequests      = "engine.requests"
	ChanIntegrationRequests = "integration.requests"
	ChanEventPublish        = "event.publish"
	ChanConfig              = "config"
	ChanTelemetry           = "telemetry"
	ChanReady               = "lifecycle.ready"
	ChanShutdown            = "lifecycle.shutdown"
)

// Wiring holds all the typed channels that connect components.
// Created once in main.go and passed to components during construction.
//
// Channel directions:
//
//	DbRequests:          Engine -> Database
//	EngineRequests:      gRPC/HTTP -> Engine
//	IntegrationRequests: Engine -> Integration Dispatcher
//	EventPublishCh:      Any component -> Event Bus
//	ConfigCh:            Config component -> All components (fanout)
//	TelemetryCh:         All components -> Telemetry component
//	ReadyCh:             Components -> main (readiness signals)
//	ShutdownCh:          main -> Components (shutdown coordination)
type Wiring struct {
	Config ChannelConfig

	// Point-to-point channels (hot paths)
	DbRequests          *Channel[Request]
	EngineRequests      *Channel[Request]
	IntegrationRequests *Channel[Request]

	// Bus channels (fanout / collection)
	EventPublishCh *Channel[*busv1.InternalMessage]
	ConfigCh       *Channel[*busv1.InternalMessage]
	TelemetryCh    *Channel[*busv1.InternalMessage]

	// Lifecycle channels
	ReadyCh    *Channel[*busv1.InternalMessage]
	ShutdownCh *Channel[*busv1.InternalMessage]
}

// NewWiring creates all channels with the given config.
func NewWiring(cfg ChannelConfig) *Wiring {
	return &Wiring{
		Config:              cfg,
		DbRequests:          NewChannel[Request](ChanDbRequests, cfg),
		EngineRequests:      NewChannel[Request](ChanEngineRequests, cfg),
		IntegrationRequests: NewChannel[Request](ChanIntegrationRequests, cfg),
		EventPublishCh:      NewChannel[*busv1.InternalMessage](ChanEventPublish, cfg),
		ConfigCh:            NewChannel[*busv1.InternalMessage](ChanConfig, cfg),
		TelemetryCh:         NewChannel[*busv1.InternalMessage](ChanTelemetry, cfg),
		ReadyCh:             NewChannel[*busv1.InternalMessage](ChanReady, cfg),
		ShutdownCh:          NewChannel[*busv1.InternalMessage](ChanShutdown, cfg),
	}
}

// Channels returns all channels as a slice for telemetry collection.
func (w *Wiring) Channels() []ChannelInfo {
	return []ChannelInfo{
		{Name: w.DbRequests.Name, Fill: w.DbRequests.Fill(), Cap: w.DbRequests.Cap(), Sent: w.DbRequests.Sent(), Dropped: w.DbRequests.Dropped()},
		{Name: w.EngineRequests.Name, Fill: w.EngineRequests.Fill(), Cap: w.EngineRequests.Cap(), Sent: w.EngineRequests.Sent(), Dropped: w.EngineRequests.Dropped()},
		{Name: w.IntegrationRequests.Name, Fill: w.IntegrationRequests.Fill(), Cap: w.IntegrationRequests.Cap(), Sent: w.IntegrationRequests.Sent(), Dropped: w.IntegrationRequests.Dropped()},
		{Name: w.EventPublishCh.Name, Fill: w.EventPublishCh.Fill(), Cap: w.EventPublishCh.Cap(), Sent: w.EventPublishCh.Sent(), Dropped: w.EventPublishCh.Dropped()},
		{Name: w.ConfigCh.Name, Fill: w.ConfigCh.Fill(), Cap: w.ConfigCh.Cap(), Sent: w.ConfigCh.Sent(), Dropped: w.ConfigCh.Dropped()},
		{Name: w.TelemetryCh.Name, Fill: w.TelemetryCh.Fill(), Cap: w.TelemetryCh.Cap(), Sent: w.TelemetryCh.Sent(), Dropped: w.TelemetryCh.Dropped()},
	}
}

// ChannelInfo holds snapshot metrics for a single channel.
type ChannelInfo struct {
	Name    string
	Fill    int
	Cap     int
	Sent    int64
	Dropped int64
}

// Close is a no-op placeholder. Channels are garbage collected when all
// references are dropped. Components should drain their input channels
// during shutdown and send DrainComplete before exiting.
func (w *Wiring) Close() {
	// Channels are not explicitly closed here because closing a channel
	// that a sender still references causes a panic. Instead, components
	// use context cancellation to stop reading, and senders check context
	// before sending.
}
