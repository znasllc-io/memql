package node

// forward_inflight.go -- the receiving replica's table of the forwarded calls
// it is serving, so that a CANCEL always finds its call.
//
// THE RACE THIS CLOSES. A forwarded call -- a tool dispatch, a model call, a
// pull, a probe, an app session -- runs on its own goroutine so it never holds
// the peer's receive loop. Its cancel arrives on the same stream right behind
// it, and it used to be matched only against a table the HANDLER fills, from
// that goroutine, once the goroutine got round to running. A cancel read first
// found nothing and was dropped, and the call then ran to its ceiling: for an
// app call, a Claude Code session on somebody's laptop, spending their
// subscription for a caller that had already given up.
//
// So the RECEIVE LOOP registers the call, synchronously, before it spawns the
// goroutine, and hands the call a context this table cancels. A cancel read
// after its request always finds it.
//
// A cancel read BEFORE its request is remembered for a while, and the request
// it names is not run at all. That order is real: the sender posts cancels on
// the peer's general outbox, so that a cancel survives a reconnect, and the
// connection's send loop drains that outbox and the attempt's own request
// queue in whichever order a select picks.
//
// Keyed by (family, peer, request id): the families have separate id spaces,
// and a cancel names a request FROM ITS OWN PEER, so another peer sending the
// same id cannot end or pre-empt somebody else's call.

import (
	"context"
	"sync"
	"time"
)

// The forward families. Each has its own id space on the sender.
const (
	forwardFamilyWorker     = "worker"
	forwardFamilyModel      = "model"
	forwardFamilyModelPull  = "modelPull"
	forwardFamilyModelProbe = "modelProbe"
	forwardFamilyApp        = "app"
)

// forwardCancelMemory is how long a cancel for a request this node has not
// seen is remembered. Far longer than a request can trail its cancel on one
// connection, and short enough that the table never matters.
const forwardCancelMemory = 2 * time.Minute

// forwardCancelMemoryMax bounds the remembered cancels, so a peer sending
// cancels for requests that never come cannot grow the table without limit.
const forwardCancelMemoryMax = 4096

type forwardCallKey struct {
	family    string
	peerId    string
	requestId string
}

type forwardCallEntry struct {
	cancel context.CancelFunc
}

// forwardCalls is usable as a zero value, because nodeService is built as a
// struct literal in more than one place.
type forwardCalls struct {
	mu        sync.Mutex
	running   map[forwardCallKey]*forwardCallEntry
	cancelled map[forwardCallKey]time.Time
	now       func() time.Time
}

func (f *forwardCalls) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// begin registers one forwarded call, ON THE RECEIVE LOOP, before it runs.
//
// It returns the context the call must run under -- derived from base and
// ended by a cancel for this request -- and the function to call when the call
// returns. ok is false when a cancel for this request already arrived: the
// caller must not run it.
func (f *forwardCalls) begin(base context.Context, family, peerId, requestId string) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(base)
	if requestId == "" {
		// Nothing can name it, so nothing can cancel it but its stream.
		return ctx, cancel, true
	}
	key := forwardCallKey{family: family, peerId: peerId, requestId: requestId}

	f.mu.Lock()
	defer f.mu.Unlock()
	if at, ok := f.cancelled[key]; ok {
		delete(f.cancelled, key)
		if f.clock().Sub(at) <= forwardCancelMemory {
			cancel()
			return ctx, func() {}, false
		}
	}
	entry := &forwardCallEntry{cancel: cancel}
	if f.running == nil {
		f.running = make(map[forwardCallKey]*forwardCallEntry)
	}
	f.running[key] = entry
	return ctx, func() {
		f.mu.Lock()
		if f.running[key] == entry {
			delete(f.running, key)
		}
		f.mu.Unlock()
		cancel()
	}, true
}

// cancel ends the named call if it is running here, and otherwise remembers
// the cancel so the request, if it arrives later, is not run. It reports
// whether a running call was found.
func (f *forwardCalls) cancel(family, peerId, requestId string) bool {
	if requestId == "" {
		return false
	}
	key := forwardCallKey{family: family, peerId: peerId, requestId: requestId}

	f.mu.Lock()
	entry, running := f.running[key]
	if running {
		delete(f.running, key)
	} else {
		f.rememberLocked(key)
	}
	f.mu.Unlock()

	if running {
		entry.cancel()
	}
	return running
}

// rememberLocked records a cancel for a request not seen yet, forgetting the
// expired ones and, at the bound, the oldest.
func (f *forwardCalls) rememberLocked(key forwardCallKey) {
	now := f.clock()
	if f.cancelled == nil {
		f.cancelled = make(map[forwardCallKey]time.Time)
	}
	for k, at := range f.cancelled {
		if now.Sub(at) > forwardCancelMemory {
			delete(f.cancelled, k)
		}
	}
	for len(f.cancelled) >= forwardCancelMemoryMax {
		var oldest forwardCallKey
		var oldestAt time.Time
		first := true
		for k, at := range f.cancelled {
			if first || at.Before(oldestAt) {
				oldest, oldestAt, first = k, at, false
			}
		}
		delete(f.cancelled, oldest)
	}
	f.cancelled[key] = now
}
