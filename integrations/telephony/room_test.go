package telephony

import "testing"

func TestRoomPrefixForPartition(t *testing.T) {
	got := RoomPrefixForPartition("acme")
	if got != "tel-acme-" {
		t.Fatalf("RoomPrefixForPartition = %q, want tel-acme-", got)
	}
}

func TestIsTelephonyRoom(t *testing.T) {
	cases := map[string]bool{
		"tel-acme-abc123":  true,
		"tel-default-x":     true,
		"polyphon-space123": false,
		"random":            false,
		"":                  false,
	}
	for room, want := range cases {
		if got := IsTelephonyRoom(room); got != want {
			t.Errorf("IsTelephonyRoom(%q) = %v, want %v", room, got, want)
		}
	}
}
