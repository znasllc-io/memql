package calls

import (
	"reflect"
	"testing"
)

func TestGrantForMatchesTheDesignTable(t *testing.T) {
	all := []string{"microphone", "camera", "screen_share", "screen_share_audio"}
	cases := []struct {
		kind SeatKind
		want GrantSpec
	}{
		{KindPerson, GrantSpec{RoomJoin: true, CanPublish: true, CanSubscribe: true, CanPublishData: true, Sources: all}},
		{KindAgent, GrantSpec{RoomJoin: true, CanPublish: true, CanSubscribe: true, CanPublishData: true, Sources: all}},
		{KindAvatar, GrantSpec{RoomJoin: true, CanPublish: true, CanSubscribe: true, CanPublishData: true, Sources: []string{"microphone", "camera"}}},
		{KindSampler, GrantSpec{RoomJoin: true, CanSubscribe: true, Hidden: true, Sources: []string{}}},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			got, err := GrantFor(c.kind)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			if got.CanUpdateOwnMetadata {
				t.Fatal("no kind may rename itself")
			}
		})
	}
}

func TestGrantForRefusesUnknownKinds(t *testing.T) {
	for _, k := range []SeatKind{"", "host", "PERSON"} {
		if _, err := GrantFor(k); err == nil {
			t.Fatalf("kind %q must be refused", k)
		}
	}
}

func TestGrantForReturnsAFreshSourcesSlice(t *testing.T) {
	a, _ := GrantFor(KindPerson)
	a.Sources[0] = "tampered"
	b, _ := GrantFor(KindPerson)
	if b.Sources[0] != "microphone" {
		t.Fatal("a caller changed the shared table")
	}
}
