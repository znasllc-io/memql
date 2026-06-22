package telephony

import "strings"

// TelephonyRoomPrefix marks a LiveKit room as a telephony (PSTN) room. The
// voice-agent dispatcher serves these the same way it serves product rooms, so
// the existing realtime agent answers phone calls. Telephony is core: the room
// is scoped by a generic partition, never a CoPresent space (Amendment A).
const TelephonyRoomPrefix = "tel-"

// RoomPrefixForPartition is the per-partition room prefix handed to a LiveKit
// SIP Individual dispatch rule. Each inbound call lands in its own room
// `tel-<partitionId>-<auto>` so callers are never bridged into a shared party
// line. The authoritative partition for a call is resolved data-driven from
// the participant metadata / the v1:telephony:number row, not by parsing this
// name -- the prefix only scopes + groups rooms for discovery.
func RoomPrefixForPartition(partitionID string) string {
	return TelephonyRoomPrefix + partitionID + "-"
}

// IsTelephonyRoom reports whether a LiveKit room name is a telephony room.
func IsTelephonyRoom(name string) bool {
	return strings.HasPrefix(name, TelephonyRoomPrefix)
}
