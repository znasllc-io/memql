// Package grpclimits holds the one number both ends of the browser path agree
// on: how large a single message on MemqlService.Stream may be.
//
// It is a package of its own because the two ends live in different modules
// that must not import each other. The gRPC server (component/grpc) sends the
// messages; the WebSocket bridge (component/server/memqlws) receives them on
// behalf of every browser. If the two numbers drift, the failure is not a
// refused request. It is a dropped session: gRPC's receive limit is a read
// error on the bridge's stream, and the bridge ends the whole WebSocket on
// any read error, so every request in flight on that browser tab goes with
// it. That is why the limit is stated once, here, and imported by both.
package grpclimits

// MaxMessageBytes is the largest single message the stream carries in either
// direction: 32 MiB.
//
// gRPC's own default is 4 MiB, which was too tight for the first thing that
// outgrew it. A worker's screenshot is a base64 PNG of the user's screen, and
// on a retina display that is commonly 5 to 10 MiB. Thirty-two MiB covers a 6K
// capture (about 28 MiB of base64) with room for the envelope. A template
// answer is the second thing to outgrow it, and the bridge shares the number
// so that a message the server is willing to send is always a message the
// bridge is willing to receive.
//
// A message over this size is never sent. The server measures an answer
// before it writes it and replies to that one request with a sentence naming
// the size and the limit, because handing gRPC an oversize message does not
// fail the call: it ends the stream.
const MaxMessageBytes = 32 * 1024 * 1024
