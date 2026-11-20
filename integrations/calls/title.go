package calls

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// checkTitle is shared by instant calls, bookings and renames. The byte cap is
// checked before normalization so combining marks cannot make a tiny visible
// title an unbounded request. Titles are plain text, never an authority or HTML.
func checkTitle(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", callrules.Refuse(callrules.ErrInvalid, "title_invalid", "a title must be valid UTF-8")
	}
	if len(raw) > 2048 {
		return "", callrules.Refuse(callrules.ErrInvalid, callrules.ReasonTitleTooLong, "a title is at most 2048 UTF-8 bytes")
	}
	title := strings.Join(strings.FieldsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if uniseg.GraphemeClusterCount(title) > 80 {
		return "", callrules.Refuse(callrules.ErrInvalid, callrules.ReasonTitleTooLong, "a title is at most 80 characters")
	}
	return title, nil
}

func callTitle(title string, at time.Time) string {
	if title != "" {
		return title
	}
	return "Call · " + at.UTC().Format("2 Jan 2006")
}

// RenameCall requires the actual occupying host seat, rechecked under the room
// gate. A bearer guest, a former host and a forged Session field cannot rename.
func (s *Service) RenameCall(ctx context.Context, sess callrules.Session, raw, identity string) (string, error) {
	if !callrules.InvitationIdentityWellFormed(identity) {
		return "", callrules.ErrInvalid
	}
	title, err := checkTitle(raw)
	if err != nil {
		return "", err
	}
	h, err := s.hostOf(ctx, sess)
	if err != nil {
		return "", err
	}
	if h.seat.Identity != identity {
		return "", notHost()
	}
	release, err := s.gates(ctx, endGates(h.room)...)
	if err != nil {
		return "", err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	current, err := s.hostOf(fresh, sess)
	if err != nil {
		return "", err
	}
	if current.seat.ID != h.seat.ID || current.seat.Identity != identity || current.room.ID != h.room.ID {
		return "", notHost()
	}
	room, err := s.stillHost(fresh, current)
	if err != nil {
		return "", err
	}
	title = callTitle(title, room.OpenedAt)
	if room.Title == title {
		return title, nil
	}
	if err := s.store.updateRoom(fresh, h.site.OwnerUserID, room, map[string]any{"title": title}, memql.VersionTimeAfter(room.Version, s.now())); err != nil {
		return "", err
	}
	return title, nil
}
