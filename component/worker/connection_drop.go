package worker

// A machine's dropped connections (network-aware fleet selection, design record
// docs/superpowers/specs/2026-11-24-network-aware-fleet-selection-design.md).
//
// WHY THIS EXISTS. A pipeline step on a fleet machine dies with the machine's
// stream: Cockpit stops every tool call in flight when the stream is lost, and
// the cluster answers the step pipeline_node_lost. A machine on weak Wi-Fi
// loses its stream again and again, and until this existed the cluster kept no
// record of it -- the cause was in a log line on the agent replica and in
// worker.log on the machine -- so placement sent 40-minute steps to it as
// readily as to a wired machine. The replica that held the stream now records
// each drop, as one v1:worker:connectionDrop row, and placement reads them
// (integrations/agent/worker connection.go).
//
// WHAT COUNTS. Only an end that cannot have been Cockpit's own choice, decided
// from what the holding replica alone knows when the stream ends: see
// classifyStreamEnd. The cluster's own reasons do not separate a lost link from
// a deliberate reconnect -- both log as "context canceled" or "superseded" --
// so the rule rests on two facts that do.

import (
	"context"
	"fmt"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The two causes a v1:worker:connectionDrop row records (its `cause` enum).
const (
	// DropCauseReplaced: a new connection for the same registration replaced
	// this one, on this replica or another, before the cluster saw it end.
	DropCauseReplaced = "replaced"
	// DropCauseWorkInFlight: the stream ended with work in flight and no
	// cluster decision behind it.
	DropCauseWorkInFlight = "workInFlight"
)

// ConnectionDrop is one dropped connection, as the replica that held the stream
// records it and as a reader gets it back.
type ConnectionDrop struct {
	RegistrationId, OwnerUserId string
	// DroppedAt is when the holding replica saw the stream end, on its clock.
	DroppedAt time.Time
	// ConnectedAt is when that stream was admitted; zero when unknown.
	ConnectedAt time.Time
	// Cause is DropCauseReplaced or DropCauseWorkInFlight.
	Cause string
	// WorkInFlight is the count of live work when the stream ended.
	WorkInFlight int
	// NodeId is the replica that held the stream; Code and Reason are what its
	// "worker disconnected" log line says (disconnectCodeReason).
	NodeId, Code, Reason string
}

// ConnectionDropRecorder writes one drop. The stream session finds it by a type
// assertion on its Store rather than through the Store interface, so the many
// test stores of this package need not grow a method they never exercise; the
// cost of that choice is that a production store lacking it would record
// nothing, silently, which is what the assertion below EngineStore holds
// against.
type ConnectionDropRecorder interface {
	RecordConnectionDrop(ctx context.Context, drop ConnectionDrop) error
}

var _ ConnectionDropRecorder = (*EngineStore)(nil)

// RecordConnectionDrop persists one v1:worker:connectionDrop row under the
// machine owner's actor, as every registration write in this store runs
// (ownerActor): the concept is owner-tiered, and the owner field is stamped from
// the actor by the mutation, never passed as data. Times travel as RFC3339Nano
// UTC; a zero ConnectedAt is left out rather than written as a fake instant.
func (s *EngineStore) RecordConnectionDrop(ctx context.Context, drop ConnectionDrop) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	writeCtx, err := ownerActor(ctx, drop.OwnerUserId)
	if err != nil {
		return err
	}
	args := map[string]any{
		"dropId":         randomHex(12),
		"registrationId": drop.RegistrationId,
		"droppedAt":      drop.DroppedAt.UTC().Format(time.RFC3339Nano),
		"cause":          drop.Cause,
		"workInFlight":   drop.WorkInFlight,
		"nodeId":         drop.NodeId,
		"code":           drop.Code,
		"reason":         drop.Reason,
	}
	if !drop.ConnectedAt.IsZero() {
		args["connectedAt"] = drop.ConnectedAt.UTC().Format(time.RFC3339Nano)
	}
	query, err := langparser.RenderCall("recordWorkerConnectionDrop", args)
	if err != nil {
		return fmt.Errorf("worker.store: render connection drop: %w", err)
	}
	if _, err := s.Engine.Execute(writeCtx, query); err != nil {
		return fmt.Errorf("worker.store: record connection drop: %w", err)
	}
	return nil
}
