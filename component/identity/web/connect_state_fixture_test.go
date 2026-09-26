package web

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// fixtureCreatedAt turns a fake row's "createdAt" into the node intrinsic the
// real engine returns it as: a state read's payload carries no createdAt key,
// and consume's lifetime check reads the node's.
func fixtureCreatedAt(v string) *timestamppb.Timestamp {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}
