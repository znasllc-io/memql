package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

var ErrFleetEnrollmentDenied = errors.New("only owners and developers can enroll their own machines")

// FleetEnrollmentOwner checks the current person at both credential creation
// and pairing redemption. A role cached in a JWT or a pairing code cannot
// keep enrollment authority after that person's role or active state changes.
func (s *Store) FleetEnrollmentOwner(ctx context.Context, callerId, requestedOwner string) (string, error) {
	callerId = strings.TrimSpace(callerId)
	requestedOwner = strings.TrimSpace(requestedOwner)
	if auth.NamesNoPerson(callerId) || (requestedOwner != "" &&
		memqlengine.BareShortId(requestedOwner) != memqlengine.BareShortId(callerId)) {
		return "", ErrFleetEnrollmentDenied
	}
	ctx = memqlengine.ContextWithFreshRead(ctx)
	person, err := s.LookupUserById(ctx, callerId)
	if err != nil {
		return "", fmt.Errorf("verify Fleet enrollment access: %w", err)
	}
	if person == nil || !person.Active || !auth.MayManageFleet(auth.Role(person.Role)) {
		return "", ErrFleetEnrollmentDenied
	}
	subject := auth.Subject{UserId: callerId, Role: auth.Role(person.Role)}
	if source := auth.InstalledMembershipSource(); source != nil {
		subject.GroupIds = source.ActiveGroupIdsForUser(ctx, callerId)
	}
	if !auth.CapableFor(ctx, subject, auth.VerbRead, "app:fleet") {
		return "", ErrFleetEnrollmentDenied
	}
	return callerId, nil
}
