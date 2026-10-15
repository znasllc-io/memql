package memqlws

// A session that has not signed in keeps the small frame limit.
//
// The bridge takes a request as large as the server's cap (32 MiB) because a
// template save needs it, and a session that has not signed in cannot save one:
// with public reads enabled (epic memql#4541) it is the anonymous actor, held to
// public-tier reads and graph subscriptions. It keeps the limit the bridge had
// before, 5 MiB, so that what a stranger can make the bridge read and hold is that
// and not the cap. The bridge knows at the upgrade: the request carries an identity
// the auth middleware verified, or it carries none.

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestASessionThatHasNotSignedInKeepsTheSmallFrameLimit(t *testing.T) {
	t.Setenv(auth.PublicReadsEnabledEnv, "true")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 6 MiB: over what a stranger may send, under what a signed-in session may.
	const queryBytes = 6 << 20

	t.Run("signed in, it is taken", func(t *testing.T) {
		svc := &stubMemqlService{}
		conn := dialBridge(t, ctx, startBridge(t, Options{}, svc))
		messageID, requestID := uuid.NewString(), uuid.NewString()
		writeStreamed(t, ctx, conn, queryRequest(messageID, requestID, queryBytes))
		requireAnswered(t, ctx, conn, requestID)
		queries := receivedQueries(svc)
		require.Len(t, queries, 1)
		require.Len(t, queries[0], queryBytes, "the server must receive the request whole")
	})

	t.Run("not signed in, it is refused by itself", func(t *testing.T) {
		svc := &stubMemqlService{}
		conn := dialBridge(t, ctx, startBridgeWith(t, Options{}, svc, bridgeSetup{anonymous: true}))
		messageID, requestID := uuid.NewString(), uuid.NewString()
		size := writeStreamed(t, ctx, conn, queryRequest(messageID, requestID, queryBytes))
		detail := requireRefused(t, ctx, conn, messageID, requestID)
		require.Equal(t, "This request is too large to send: it is 6.1 MiB and one request can carry at most 5 MiB. Send less at once and try again.",
			detail["message"])
		require.Equal(t, map[string]any{"code": "REQUEST_TOO_LARGE", "sizeBytes": strconv.FormatInt(size, 10), "limitBytes": "5242880"}, detail["metadata"])
		require.Empty(t, receivedQueries(svc), "the server must never see a request the bridge refused")
		requireTheSessionCarriesOn(t, ctx, conn)

		// The library reads and discards up to twice the limit, and closes the
		// connection past that: twice 5 MiB, not twice the cap.
		writeStreamed(t, ctx, conn, queryRequest(uuid.NewString(), uuid.NewString(), 11<<20))
		var frame map[string]any
		err := wsjson.Read(ctx, conn, &frame)
		require.Error(t, err, "a request of 11 MiB from a session that has not signed in was answered: %v", frame)
		require.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
	})

	t.Run("not signed in, a smaller limit the operator set still holds", func(t *testing.T) {
		svc := &stubMemqlService{}
		conn := dialBridge(t, ctx, startBridgeWith(t, Options{MaxMessageBytes: 64 << 10}, svc, bridgeSetup{anonymous: true}))
		messageID, requestID := uuid.NewString(), uuid.NewString()
		writeStreamed(t, ctx, conn, queryRequest(messageID, requestID, 100<<10))
		detail := requireRefused(t, ctx, conn, messageID, requestID)
		require.Equal(t, "65536", detail["metadata"].(map[string]any)["limitBytes"])
		requireTheSessionCarriesOn(t, ctx, conn)
	})
}

// queryRequest reads as an executeQuery whose query is queryBytes q's, without the
// request being held.
func queryRequest(messageID, requestID string, queryBytes int) io.Reader {
	return io.MultiReader(
		strings.NewReader(`{"executeQuery":{"requestId":"`+requestID+`","query":"`),
		&repeatReader{text: strings.Repeat("q", 4096), left: queryBytes},
		strings.NewReader(`"},"messageId":"`+messageID+`"}`),
	)
}

// writeStreamed sends what r reads as one message, and returns its size.
func writeStreamed(t *testing.T, ctx context.Context, conn *websocket.Conn, r io.Reader) int64 {
	t.Helper()
	writer, err := conn.Writer(ctx, websocket.MessageText)
	require.NoError(t, err)
	n, err := io.Copy(writer, r)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return n
}
