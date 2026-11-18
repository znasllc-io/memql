package calls

import "testing"

func TestDecideJoinFollowsThePolicy(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		name string
		room RoomState
		ask  JoinAsk
		free int
		want Outcome
		why  string
	}{
		{"ended refuses", RoomState{Ended: true}, JoinAsk{Kind: KindPerson}, 5, OutcomeRefuse, "call_ended"},
		{"ended refuses a rejoin too", RoomState{Ended: true}, JoinAsk{Kind: KindPerson, Rejoin: true}, 5, OutcomeRefuse, "call_ended"},
		{"rejoin skips counts", RoomState{People: 8, Locked: true}, JoinAsk{Kind: KindPerson, Rejoin: true}, 0, OutcomeAdmit, ""},
		{"people full", RoomState{People: 8}, JoinAsk{Kind: KindPerson}, 5, OutcomeRefuse, "room_full"},
		{"agents full", RoomState{Agents: 2}, JoinAsk{Kind: KindAgent}, 5, OutcomeRefuse, "agent_seats_full"},
		{"agent with people full is fine", RoomState{People: 8, Agents: 1}, JoinAsk{Kind: KindAgent}, 5, OutcomeAdmit, ""},
		{"locked waits", RoomState{Locked: true}, JoinAsk{Kind: KindPerson}, 5, OutcomeWait, "locked"},
		{"locked admits host", RoomState{Locked: true}, JoinAsk{Kind: KindPerson, Host: true}, 5, OutcomeAdmit, ""},
		{"no free seats queues", RoomState{}, JoinAsk{Kind: KindPerson}, 0, OutcomeQueue, "capacity"},
		{"free seats admit", RoomState{People: 3}, JoinAsk{Kind: KindPerson}, 1, OutcomeAdmit, ""},
		{"full outranks lock", RoomState{People: 8, Locked: true}, JoinAsk{Kind: KindPerson}, 0, OutcomeRefuse, "room_full"},
		{"lock outranks capacity", RoomState{Locked: true}, JoinAsk{Kind: KindPerson}, 0, OutcomeWait, "locked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecideJoin(c.room, c.ask, p, c.free)
			if got.Outcome != c.want || got.Reason != c.why {
				t.Fatalf("got %+v, want %s/%q", got, c.want, c.why)
			}
		})
	}
}

func TestDefaultPolicyValues(t *testing.T) {
	p := DefaultPolicy()
	if p.MaxPeople != 8 || p.MaxAgents != 2 || p.RoomJoinsPerMinute != 30 ||
		p.MaxUpcomingScheduled != 20 || p.MaxScheduledMinutes != 480 {
		t.Fatalf("unexpected counts: %+v", p)
	}
	if p.HostLeftGrace.Minutes() != 5 || p.PassTTL.Seconds() != 120 ||
		p.ScheduleHorizon.Hours() != 720 || p.QueueEntryTTL.Minutes() != 10 || p.QueuePoll.Seconds() != 3 {
		t.Fatalf("unexpected durations: %+v", p)
	}
}
