package main

import (
	"context"
	"errors"
	"strconv"

	"github.com/znasllc-io/memql/sdk/go/client"
)

type processingClient interface {
	CallsProcessingRooms(context.Context, client.CallsProcessingRoomsArgs) (*client.Result, error)
	CallsProcessingLease(context.Context, client.CallsProcessingLeaseArgs) (*client.Result, error)
	CallsProcessingPublish(context.Context, client.CallsProcessingPublishArgs) (*client.Result, error)
}

func connect(ctx context.Context, cfg client.ConnectConfig) (operation, func(), error) {
	bounded, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	conn, err := client.Connect(bounded, cfg)
	if err != nil {
		return nil, nil, err
	}
	return sdkOperation(client.NewQueryClient(conn.Dispatcher())), conn.Close, nil
}

func sdkOperation(q processingClient) operation {
	return func(ctx context.Context, req request) (any, error) {
		var result *client.Result
		var err error
		switch req.Action {
		case "rooms":
			result, err = q.CallsProcessingRooms(ctx, client.CallsProcessingRoomsArgs{SiteId: req.SiteID})
		case "lease":
			args := client.CallsProcessingLeaseArgs{SiteId: req.SiteID, RoomId: req.RoomID, LeaseId: req.LeaseID, MaxTracks: 100}
			if req.Reset != nil {
				args.Reset, args.ResetSet = *req.Reset, true
			}
			if req.MaxTracks != nil {
				args.MaxTracks = *req.MaxTracks
			}
			result, err = q.CallsProcessingLease(ctx, args)
		case "publish":
			if req.Revision == nil || strconv.IntSize < 64 && *req.Revision > 2147483647 {
				return nil, errors.New("invalid_revision")
			}
			result, err = q.CallsProcessingPublish(ctx, client.CallsProcessingPublishArgs{SiteId: req.SiteID, RoomId: req.RoomID, LeaseId: req.LeaseID, Revision: int(*req.Revision), Event: req.Event})
		default:
			return nil, errors.New("invalid_action")
		}
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, errors.New("invalid_response")
		}
		rows := result.Rows()
		if len(rows) != 1 || len(rows[0]) == 0 {
			return nil, errors.New("invalid_response")
		}
		return rows[0], nil
	}
}
