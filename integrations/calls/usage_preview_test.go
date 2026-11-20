package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestUsagePreviewStoredLimitsFailClosed(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	valid := roomRow{OpenedAt: now, roomFunding: roomFunding{FundingMode: usagePreviewMode, FundingDeadline: now.Add(usagePreviewDuration), FundingMaxPeople: 8}}
	require.NoError(t, fundingRoomAllowed(valid, now))
	for name, change := range map[string]func(*roomRow){
		"unknown mode":      func(r *roomRow) { r.FundingMode = "future_unbounded" },
		"mixed grants":      func(r *roomRow) { r.FundingPassID = "legacy-pass" },
		"missing deadline":  func(r *roomRow) { r.FundingDeadline = time.Time{} },
		"extended deadline": func(r *roomRow) { r.FundingDeadline = r.FundingDeadline.Add(time.Second) },
		"expanded capacity": func(r *roomRow) { r.FundingMaxPeople = 9 },
		"missing origin":    func(r *roomRow) { r.OpenedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			room := valid
			change(&room)
			require.True(t, room.bounded())
			require.Equal(t, "allowance_unavailable", callrules.ReasonOf(fundingRoomAllowed(room, now)))
		})
	}
	require.Equal(t, endReasonAllowance, callrules.ReasonOf(fundingRoomAllowed(valid, valid.FundingDeadline)))
	policy := callrules.DefaultPolicy()
	policy.MaxPeople = 100
	policy.MaxAgents = 100
	bounded := fundedPolicy(policy, valid.roomFunding)
	require.Equal(t, 8, bounded.MaxPeople)
	require.Zero(t, bounded.MaxAgents)
	require.False(t, callrules.DefaultPolicy().UsagePreview)
}
