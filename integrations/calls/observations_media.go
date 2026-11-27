package calls

import (
	"context"
	"errors"

	"github.com/livekit/protocol/livekit"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// A fixed server-origin topic forwards only explicitly authorized text. It does
// not create subscriptions, a sampler, media passes or provider/device access.
func (b *liveKitBackend) PublishObservation(ctx context.Context, room string, data []byte, recipients []string) error {
	sender, ok := b.rooms.(interface {
		SendData(context.Context, *livekit.SendDataRequest) (*livekit.SendDataResponse, error)
	})
	if !ok || len(recipients) < 2 || len(recipients) > 8 || len(data) > 8192 {
		return errors.New("calls: observation forwarding unavailable")
	}
	topic := callrules.ObservationProtocol
	_, e := sender.SendData(ctx, &livekit.SendDataRequest{Room: room, Data: data, Kind: livekit.DataPacket_RELIABLE, DestinationIdentities: recipients, Topic: &topic})
	return e
}
