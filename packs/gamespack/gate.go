package gamespack

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// gate.go -- the cross-replica write gate every read-modify-write of a games
// row takes: recording a trace (keyed on the trace id, so that looking at who holds
// it and writing it are one step), learning (keyed on the owner and the goal
// signature) and every ladder move (keyed on the skill). A Postgres lock around the
// read and the write, a read that skips this node's result cache, and a version
// stamped after the one it read -- each alone was measured insufficient
// (memql#5431).
//
// A GATE IS HELD FOR ONE CRITICAL SECTION AND NEVER ACROSS A MODEL CALL. Gates are
// taken on a small pool of their own (four connections by default), and a caller
// waiting for a lock holds its connection for up to six seconds: a gate held across
// an embedding leaves every other caller's gate unable to be taken.

// gate takes the cross-replica write gate for key and returns its release.
func (p *Provider) gate(ctx context.Context, key string) (func(), error) {
	if p.lock != nil {
		return p.lock(ctx, key)
	}
	pool, err := memql.ResolveWriteGatePool(p.gates, "games", "PluginContext.GateDB")
	if err != nil {
		return nil, err
	}
	return memql.AcquireWriteGate(ctx, pool, key)
}
