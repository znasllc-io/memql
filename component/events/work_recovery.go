package events

// TopicWorkRunRecovery is an internal scheduling hint, not a graph update or
// a grant. Consumers re-read the owned run and verify its exact stale snapshot
// before competing for its existing execution claim. RunDelivery retains it
// across a mesh interruption just like run lifecycle events.
const TopicWorkRunRecovery = "work.run.recovery"
