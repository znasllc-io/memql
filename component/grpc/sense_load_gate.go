package memql

import (
	"context"
	"sync"
)

// sense_load_gate.go -- at most one Sense load pass in flight per stream
// (memql#5434).
//
// A Diagnose that carries a file_path also runs the engine's load over the
// source, which costs up to ~100 ms on the largest tree files. Every Sense
// request is answered on a goroutine of its own, so without a bound a client
// sending Diagnose on every keystroke -- any authenticated stream may -- would
// run a load per keystroke, concurrently, for as long as it typed.
//
// The gate is the language server's rule applied to a stream: one pass in
// flight, and one request waiting behind it. A newer request takes the waiting
// place, and the one it displaces is answered at once with Diagnose's own
// diagnostics -- its fast answer, which nothing here delays or withholds -- and
// no load; its document has moved on, and the newer request's pass answers for
// it. A request whose stream ends while it waits runs nothing.

// senseLoadGate serializes one stream's load passes. The zero value is ready.
type senseLoadGate struct {
	mu      sync.Mutex
	running bool
	// waiting is the one request queued behind the pass in flight, nil when
	// none is.
	waiting senseLoadTurn
}

// senseLoadTurn is a queued request's answer: true when the pass in flight
// hands it the gate, false when a newer request displaced it. Buffered, so the
// decision is never lost to a receiver that has not started listening.
type senseLoadTurn chan bool

// run runs pass once the stream has no pass in flight, and reports whether it
// ran. It returns false without running pass when a newer request displaced
// this one while it waited, or when ctx -- the stream's -- ended first. pass
// receives ctx, so a pass in flight stops with the stream as well.
func (g *senseLoadGate) run(ctx context.Context, pass func(context.Context)) bool {
	g.mu.Lock()
	if !g.running {
		g.running = true
		g.mu.Unlock()
	} else {
		turn := make(senseLoadTurn, 1)
		if g.waiting != nil {
			g.waiting <- false // displaced: answered without a load
		}
		g.waiting = turn
		g.mu.Unlock()

		select {
		case ours := <-turn:
			if !ours {
				return false
			}
		case <-ctx.Done():
			g.mu.Lock()
			if g.waiting == turn {
				g.waiting = nil
				g.mu.Unlock()
				return false
			}
			g.mu.Unlock()
			// The gate decided while the stream was ending: take the
			// decision, and pass the gate on if it was ours.
			if <-turn {
				g.release()
			}
			return false
		}
	}
	defer g.release()
	if ctx.Err() != nil {
		return false
	}
	pass(ctx)
	return true
}

// release ends the pass in flight, handing the gate to the waiting request if
// there is one.
func (g *senseLoadGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.waiting != nil {
		next := g.waiting
		g.waiting = nil
		next <- true // the gate stays taken, now by the waiting request
		return
	}
	g.running = false
}
