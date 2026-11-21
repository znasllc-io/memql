package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/sdk/go/client"
)

type fakeProcessingClient struct {
	rooms   func(client.CallsProcessingRoomsArgs) (*client.Result, error)
	lease   func(client.CallsProcessingLeaseArgs) (*client.Result, error)
	publish func(client.CallsProcessingPublishArgs) (*client.Result, error)
}

func (f fakeProcessingClient) CallsProcessingRooms(_ context.Context, args client.CallsProcessingRoomsArgs) (*client.Result, error) {
	return f.rooms(args)
}

func (f fakeProcessingClient) CallsProcessingLease(_ context.Context, args client.CallsProcessingLeaseArgs) (*client.Result, error) {
	return f.lease(args)
}
func (f fakeProcessingClient) CallsProcessingPublish(_ context.Context, args client.CallsProcessingPublishArgs) (*client.Result, error) {
	return f.publish(args)
}

func TestTypedAdapterPreservesExplicitResetAndNativeData(t *testing.T) {
	q := fakeProcessingClient{lease: func(args client.CallsProcessingLeaseArgs) (*client.Result, error) {
		require.Equal(t, client.CallsProcessingLeaseArgs{SiteId: "site", RoomId: "room", ResetSet: true, Reset: false, MaxTracks: 100}, args)
		return client.ResultFromRows([]client.Row{{"leaseId": "lease", "tracks": []any{}}}), nil
	}}
	req, err := parseRequest([]byte(`{"id":"one","action":"lease","siteId":"site","roomId":"room","reset":false}`))
	require.NoError(t, err)
	result, err := sdkOperation(q)(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, client.Row{"leaseId": "lease", "tracks": []any{}}, result)
	q.lease = func(client.CallsProcessingLeaseArgs) (*client.Result, error) { return client.ResultFromRows(nil), nil }
	_, err = sdkOperation(q)(context.Background(), req)
	require.ErrorContains(t, err, "invalid_response")
}

func TestTypedPublicationNeverInterpretsContentAsAQuery(t *testing.T) {
	req, err := parseRequest([]byte(`{"id":"one","action":"publish","siteId":"site","roomId":"room","leaseId":"lease","revision":2,"event":{"text":"'); builtin unrelated({})","sequence":1}}`))
	require.NoError(t, err)
	q := fakeProcessingClient{publish: func(args client.CallsProcessingPublishArgs) (*client.Result, error) {
		require.Equal(t, int(2), args.Revision)
		require.Equal(t, req.Event, args.Event)
		// Use the actual generated builder; payload bytes remain a quoted value.
		call := client.CallsProcessingPublishBuild(args)
		require.Contains(t, call, `text: "'); builtin unrelated({})"`)
		require.Contains(t, call, `sequence: 1`)
		return client.ResultFromRows([]client.Row{{"status": "accepted"}}), nil
	}}
	result, err := sdkOperation(q)(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, client.Row{"status": "accepted"}, result)
}

func TestDiscoveryAndZeroCapacityUseTypedNativeArguments(t *testing.T) {
	q := fakeProcessingClient{
		rooms: func(args client.CallsProcessingRoomsArgs) (*client.Result, error) {
			require.Equal(t, "site", args.SiteId)
			return client.ResultFromRows([]client.Row{{"rooms": []any{}}}), nil
		},
		lease: func(args client.CallsProcessingLeaseArgs) (*client.Result, error) {
			require.Zero(t, args.MaxTracks)
			require.Contains(t, client.CallsProcessingLeaseBuild(args), "maxTracks: 0")
			return client.ResultFromRows([]client.Row{{"reason": "processing_capacity"}}), nil
		},
	}
	for _, input := range []string{`{"id":"one","action":"rooms","siteId":"site"}`, `{"id":"one","action":"lease","siteId":"site","roomId":"room","maxTracks":0}`} {
		req, err := parseRequest([]byte(input))
		require.NoError(t, err)
		_, err = sdkOperation(q)(context.Background(), req)
		require.NoError(t, err)
	}
}
