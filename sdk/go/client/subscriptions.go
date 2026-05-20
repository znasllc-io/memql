package client

import (
	"context"
	"fmt"
	"sync"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// SubscriptionManager handles event subscriptions over the gRPC stream.
// It demuxes incoming EventNotification messages by subscription_id.
type SubscriptionManager struct {
	dispatcher *Dispatcher
	mu         sync.Mutex
	subs       map[string]chan *memqlv1.EventNotification // subscription_id -> channel
	done       chan struct{}
}

// NewSubscriptionManager creates a subscription manager that reads from
// the dispatcher's event channel.
func NewSubscriptionManager(dispatcher *Dispatcher) *SubscriptionManager {
	sm := &SubscriptionManager{
		dispatcher: dispatcher,
		subs:       make(map[string]chan *memqlv1.EventNotification),
		done:       make(chan struct{}),
	}
	go sm.demux()
	return sm
}

// Subscribe sends a SubscribeMsg and returns a channel for receiving events.
func (sm *SubscriptionManager) Subscribe(ctx context.Context, kind memqlv1.SubscriptionKind, filter string) (string, <-chan *memqlv1.EventNotification, error) {
	subId := id.NewShortId()

	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_Subscribe{
			Subscribe: &memqlv1.SubscribeMsg{
				SubscriptionId: subId,
				Kind:           kind,
				Filter:         filter,
			},
		},
	}

	_, err := sm.dispatcher.Send(msg)
	if err != nil {
		return "", nil, fmt.Errorf("send subscribe: %w", err)
	}

	ch := make(chan *memqlv1.EventNotification, 64)
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

// demux reads from the dispatcher's event channel and routes EventNotifications
// to the appropriate subscription channel.
func (sm *SubscriptionManager) demux() {
	for {
		select {
		case <-sm.done:
			return
		case msg, ok := <-sm.dispatcher.Events():
			if !ok {
				return
			}
			event := msg.GetEvent()
			if event == nil {
				continue // Not an event notification (heartbeat, etc.)
			}
			subId := event.GetSubscriptionId()
			sm.mu.Lock()
			ch, exists := sm.subs[subId]
			sm.mu.Unlock()
			if exists {
				select {
				case ch <- event:
				default:
					// Drop if channel is full — subscriber should drain.
				}
			}
		}
	}
}
