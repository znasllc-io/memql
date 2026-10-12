package grpclimits

import "testing"

// MaxMessageBytes is shared by the server that sends and the bridge that
// receives for every browser, so a change to it moves both ends at once. It has
// two reasons to be what it is, and a change that breaks either should be made
// on purpose: it stays above gRPC's own 4 MiB default (the bridge raises its
// receive limit to it so that an answer between the two is carried instead of
// ending the session), and it covers the message the stream was first raised
// for, a worker's screenshot of a 6K display, about 28 MiB once encoded.
func TestMaxMessageBytesCoversWhatTheStreamWasRaisedFor(t *testing.T) {
	const (
		grpcDefault        = 4 << 20
		sixKScreenshotSize = 28 << 20
	)
	limit := MaxMessageBytes
	if limit <= grpcDefault {
		t.Errorf("the limit is %d bytes, no more than gRPC's default of %d: the bridge would carry nothing it does not already", limit, grpcDefault)
	}
	if limit < sixKScreenshotSize {
		t.Errorf("the limit is %d bytes, under the %d a 6K screenshot takes", limit, sixKScreenshotSize)
	}
}
