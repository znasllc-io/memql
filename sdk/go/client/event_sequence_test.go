package client

// The live-data continuity fields on a delivered event (memql#4536). The Go
// SDK carries them at WIRE level in this epic -- there is no Go
// LiveCollection yet (recorded in sdk/go/CLAUDE.md) -- so what is pinned here
// is that a Go consumer building its own fold can see the gap at all.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestEventFromProtoCarriesSeqAndGap(t *testing.T) {
	ev, err := eventFromProto(&memqlv1.EventNotification{
		SubscriptionId: "sub-1",
		Kind:           memqlv1.EventKind_EVENT_KIND_NODE_CREATED,
		Seq:            42,
		GapBefore:      true,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(42), ev.Seq)
	assert.True(t, ev.GapBefore)

	// An older server sends neither. Zero and false are the honest answers:
	// "this connection carries no sequence" and "nothing known to be missed".
	old, err := eventFromProto(&memqlv1.EventNotification{SubscriptionId: "sub-1"})
	require.NoError(t, err)
	assert.Equal(t, uint64(0), old.Seq)
	assert.False(t, old.GapBefore)
}
