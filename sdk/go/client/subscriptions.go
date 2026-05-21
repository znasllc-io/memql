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

// Subscribe sends a SubscribeMsg and returns a channel for receiving
// SDK-owned Event values. The caller drains the channel until it is
// closed (by Unsubscribe or Stop). `kind` is the SDK-owned
// SubscriptionKind; the proto enum stays inside the SDK.
func (sm *SubscriptionManager) Subscribe(ctx context.Context, kind SubscriptionKind, filter string) (string, <-chan Event, error) {
	subId := id.NewShortId()

	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_Subscribe{
			Subscribe: &memqlv1.SubscribeMsg{
				SubscriptionId: subId,
				Kind:           kind.toProto(),
				Filter:         filter,
			},
		},
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
