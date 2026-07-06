package client

import (
	"context"
	"fmt"
	"sync"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// SubscriptionManager handles event subscriptions over the gRPC stream.
// It demuxes incoming EventNotification messages by subscription_id and
// delivers SDK-owned Event values to subscribers.
type SubscriptionManager struct {
	dispatcher *Dispatcher
	mu         sync.Mutex
	subs       map[string]chan Event // subscription_id -> channel
	done       chan struct{}
}

// NewSubscriptionManager creates a subscription manager that reads from
// the dispatcher's event channel.
func NewSubscriptionManager(dispatcher *Dispatcher) *SubscriptionManager {
	sm := &SubscriptionManager{
		dispatcher: dispatcher,
		subs:       make(map[string]chan Event),
		done:       make(chan struct{}),
	}
	go sm.demux()
	return sm
}

// Subscribe sends a SubscribeMsg for a NON-graph subscription kind and
// returns a channel for receiving SDK-owned Event values. The caller
// drains the channel until it is closed (by Unsubscribe or Stop). `kind`
// is the SDK-owned SubscriptionKind; the proto enum stays inside the SDK.
//
// Graph subscriptions are STRUCTURED (memql#2460): the server composes the
// bus topic from a concept + actions, so a free-text filter is no longer
// accepted for SubscriptionKindGraphEvents. Passing it here is a client
// error -- use SubscribeGraph instead.
func (sm *SubscriptionManager) Subscribe(ctx context.Context, kind SubscriptionKind, filter string) (string, <-chan Event, error) {
	if kind == SubscriptionKindGraphEvents {
		return "", nil, fmt.Errorf("graph subscriptions are structured; use SubscribeGraph (concept + actions) instead of Subscribe with a free-text filter")
	}
	return sm.subscribe(ctx, &memqlv1.SubscribeMsg{
		Kind:   kind.toProto(),
		Filter: filter,
	})
}

// GraphSubscribeOptions selects which graph CDC events a structured graph
// subscription receives (memql#2460). Both fields are optional:
//
//   - Concept is a canonical concept TYPE id (e.g. "v1:cognition:utterance").
//     Empty = all concepts.
//   - Actions is the set of CDC verbs. Empty = all actions.
//
// The server composes the bus topic from these, so the client never writes
// a `graph.node.<action>.<concept>` topic string.
type GraphSubscribeOptions struct {
	Concept string
	Actions []GraphAction
}

// SubscribeGraph opens a STRUCTURED graph subscription and returns a
// channel of SDK-owned Event values. It is the graph counterpart of
// Subscribe: the server composes the bus topic from opts.Concept +
// opts.Actions (memql#2460).
func (sm *SubscriptionManager) SubscribeGraph(ctx context.Context, opts GraphSubscribeOptions) (string, <-chan Event, error) {
	var actions []memqlv1.GraphNodeAction
	if len(opts.Actions) > 0 {
		actions = make([]memqlv1.GraphNodeAction, 0, len(opts.Actions))
		for _, a := range opts.Actions {
			actions = append(actions, a.toProto())
		}
	}
	return sm.subscribe(ctx, &memqlv1.SubscribeMsg{
		Kind:    memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
		Concept: opts.Concept,
		Actions: actions,
	})
}

// subscribe stamps a fresh subscription id, sends the SubscribeMsg, and
// registers the demux channel. Shared by Subscribe + SubscribeGraph.
func (sm *SubscriptionManager) subscribe(ctx context.Context, sub *memqlv1.SubscribeMsg) (string, <-chan Event, error) {
	_ = ctx
	subId := id.NewShortId()
	sub.SubscriptionId = subId

	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_Subscribe{Subscribe: sub},
	}

	_, err := sm.dispatcher.Send(msg)
	if err != nil {
		return "", nil, fmt.Errorf("send subscribe: %w", err)
	}

	ch := make(chan Event, 64)
	sm.mu.Lock()
	sm.subs[subId] = ch
	sm.mu.Unlock()

	return subId, ch, nil
}

// Unsubscribe sends an UnsubscribeMsg and closes the event channel.
func (sm *SubscriptionManager) Unsubscribe(subId string) error {
	sm.mu.Lock()
	ch, ok := sm.subs[subId]
	if ok {
		delete(sm.subs, subId)
		close(ch)
	}
	sm.mu.Unlock()

	if !ok {
		return nil
	}

	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_Unsubscribe{
			Unsubscribe: &memqlv1.UnsubscribeMsg{
				SubscriptionId: subId,
			},
		},
	}
	_, err := sm.dispatcher.Send(msg)
	return err
}

// Stop shuts down the demux goroutine and closes all subscription channels.
func (sm *SubscriptionManager) Stop() {
	select {
	case <-sm.done:
	default:
		close(sm.done)
	}

	sm.mu.Lock()
	for id, ch := range sm.subs {
		close(ch)
		delete(sm.subs, id)
	}
	sm.mu.Unlock()
}

// demux reads from the dispatcher's event channel and routes
// EventNotifications to the appropriate subscription channel,
// converting them to the SDK-owned Event shape at the boundary.
// Events that fail payload decoding are silently dropped -- a
// malformed payload is a wire-level fault the subscriber can't act
// on anyway.
func (sm *SubscriptionManager) demux() {
	for {
		select {
		case <-sm.done:
			return
		case msg, ok := <-sm.dispatcher.Events():
			if !ok {
				return
			}
			ev := msg.GetEvent()
			if ev == nil {
				continue // Not an event notification (heartbeat, etc.)
			}
			subId := ev.GetSubscriptionId()
			sm.mu.Lock()
			ch, exists := sm.subs[subId]
			sm.mu.Unlock()
			if !exists {
				continue
			}
			wrapped, err := eventFromProto(ev)
			if err != nil {
				continue
			}
			select {
			case ch <- wrapped:
			default:
				// Drop if channel is full — subscriber should drain.
			}
		}
	}
}
