package calls

// DecideJoin answers one join request. The order of the checks is the
// contract: ended, rejoin, kind limits, lock, capacity.
//
//   - A rejoin comes right after "ended" and skips every count, because the
//     person already held a seat; a dropped connection must never turn into
//     "room full" or a queue position behind strangers.
//   - Kind limits come before the lock so a full room is reported as full
//     rather than as waiting for a host who could never let anyone in.
//   - Capacity is last: freeSeats is the caller's measure of seats the
//     platform can still give, and running out of them queues rather than
//     refuses, since it clears without the room changing.
func DecideJoin(room RoomState, ask JoinAsk, p Policy, freeSeats int) Decision {
	if room.Ended {
		return Decision{OutcomeRefuse, "call_ended"}
	}
	if ask.Rejoin {
		return Decision{Outcome: OutcomeAdmit}
	}
	switch ask.Kind {
	case KindPerson:
		if room.People >= p.MaxPeople {
			return Decision{OutcomeRefuse, "room_full"}
		}
	case KindAgent:
		if room.Agents >= p.MaxAgents {
			return Decision{OutcomeRefuse, "agent_seats_full"}
		}
	}
	if room.Locked && !ask.Host {
		return Decision{OutcomeWait, "locked"}
	}
	if freeSeats <= 0 {
		return Decision{OutcomeQueue, "capacity"}
	}
	return Decision{Outcome: OutcomeAdmit}
}
