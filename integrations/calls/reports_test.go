package calls

import (
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"strings"
	"testing"
)

func TestReportRequestClosedBounds(t *testing.T) {
	q := callrules.ReportRequest{RoomID: "room", Identity: "identity", Category: "other", Description: "A bounded benign report", IdempotencyKey: "report-request-0001"}
	require.True(t, validReportRequest("report_submit", q))
	for _, change := range []func(*callrules.ReportRequest){func(q *callrules.ReportRequest) { q.Description = strings.Repeat("界", 2001) }, func(q *callrules.ReportRequest) { q.Description = "bad\x00control" }, func(q *callrules.ReportRequest) { q.Revision = 1 }, func(q *callrules.ReportRequest) { q.ID = strings.Repeat("a", 64) }, func(q *callrules.ReportRequest) { q.Category = "guessed_child_age" }} {
		bad := q
		change(&bad)
		require.False(t, validReportRequest("report_submit", bad))
	}
	require.True(t, validReportRequest("report_list", callrules.ReportRequest{}))
	require.False(t, validReportRequest("report_list", q))
	require.Equal(t, "report", agentRequestScope(callrules.AgentRequest{Action: "report_submit", Report: &q}))
	require.Empty(t, agentRequestScope(callrules.AgentRequest{Action: "host", Report: &q}))
}
