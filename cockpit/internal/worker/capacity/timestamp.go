package capacity

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func timestamp(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
