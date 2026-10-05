package identity

import (
	"context"
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// RegistrationConfig reads the persisted policy on every decision, including
// on a replica that did not handle the settings write. Empty lists deliberately
// clear boot defaults; only an absent row uses the installation's config.
// A failed read must never reopen registration using an older boot policy.
func (s *Store) RegistrationConfig(ctx context.Context, fallback Config) (Config, error) {
	row, err := s.ReadClusterSettings(memqlengine.ContextWithFreshRead(auth.ContextWithInternalOrigin(ctx)))
	if err != nil {
		return Config{}, err
	}
	if row == nil {
		return fallback, nil
	}
	out := fallback
	out.RegistrationMode = RegistrationMode(strings.TrimSpace(row.RegistrationMode))
	if !out.RegistrationMode.IsValid() {
		return Config{}, fmt.Errorf("identity: invalid saved registration mode %q", row.RegistrationMode)
	}
	out.RegistrationDomains = registrationList(row.RegistrationDomains)
	out.InternalDomains = registrationList(row.InternalDomains)
	out.AccessRequestNotifyEmails = registrationList(row.AccessRequestNotifyEmails)
	out.InternalDefaultRole = strings.TrimSpace(row.InternalDefaultRole)
	if !auth.IsValidRole(auth.Role(out.InternalDefaultRole)) {
		return Config{}, fmt.Errorf("identity: invalid saved internal role %q", row.InternalDefaultRole)
	}
	return out, nil
}

var registrationDomainLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// The UI and API both accept exact email domains, never URLs or wildcards.
func ValidateRegistrationLists(mode, approved, internal, notifications string) error {
	if mode == "domain_restricted" && len(registrationList(approved)) == 0 {
		return fmt.Errorf("approved email domains are required")
	}
	for _, list := range []string{approved, internal} {
		for _, domain := range registrationList(list) {
			if len(domain) > 253 {
				return fmt.Errorf("email domain is too long")
			}
			for _, label := range strings.Split(domain, ".") {
				if !registrationDomainLabel.MatchString(label) {
					return fmt.Errorf("use email domains such as example.com, without @, URLs or wildcards")
				}
			}
		}
	}
	for _, email := range registrationList(notifications) {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email {
			return fmt.Errorf("notification recipients must be email addresses")
		}
	}
	return nil
}

func registrationList(value string) []string {
	var result []string
	for _, entry := range strings.Split(value, ",") {
		if entry = strings.ToLower(strings.TrimSpace(entry)); entry != "" {
			result = append(result, entry)
		}
	}
	return result
}
