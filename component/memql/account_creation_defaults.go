package memql

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

var assignedDomainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func normalizedAssignedDomain(value string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return "", fmt.Errorf("account_domain_invalid: enter a domain such as example.com")
	}
	for _, label := range strings.Split(host, ".") {
		if !assignedDomainLabel.MatchString(label) {
			return "", fmt.Errorf("account_domain_invalid: enter a domain without a scheme, path or wildcard")
		}
	}
	return host, nil
}

// Judge the merged row on every write, including callers outside the OS. The
// profile's domain and its verified access policy remain separate from defaults.
func normalizeAccountCreationDomains(payload map[string]any) error {
	raw, configured := payload["domains"]
	if !configured {
		if strings.TrimSpace(stringFromAny(payload["defaultDomain"])) != "" {
			return fmt.Errorf("account_domain_default: assign the default domain to this account first")
		}
		return nil
	}
	var values []string
	switch list := raw.(type) {
	case []string:
		values = list
	case []any:
		for _, value := range list {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("account_domain_invalid: domains must be names")
			}
			values = append(values, text)
		}
	default:
		return fmt.Errorf("account_domain_invalid: domains must be a list")
	}
	if len(values) > 32 {
		return fmt.Errorf("account_domain_limit: assign at most 32 domains")
	}
	names := make([]any, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		name, err := normalizedAssignedDomain(value)
		if err != nil {
			return err
		}
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	selected := strings.TrimSpace(stringFromAny(payload["defaultDomain"]))
	if selected != "" {
		var err error
		selected, err = normalizedAssignedDomain(selected)
		if err != nil {
			return err
		}
	}
	if len(names) > 0 && !seen[selected] || len(names) == 0 && selected != "" {
		return fmt.Errorf("account_domain_default: choose a default from this account's assigned domains")
	}
	payload["domains"], payload["defaultDomain"] = names, selected
	return nil
}

// staged-data: MUST-NOT-GATE -- a reference's organization is an invariant;
// hiding staged rows must not allow a cross-organization default sender.
func (e *MemQLEngine) validateAccountCreationDefaults(ctx context.Context, accountID string, payload map[string]any) error {
	if err := normalizeAccountCreationDomains(payload); err != nil {
		return err
	}
	id := strings.TrimSpace(stringFromAny(payload["defaultSenderIdentityId"]))
	if id == "" {
		return nil
	}
	const concept = "v1:campaigns:senderIdentity"
	db := e.database()
	if db == nil {
		return fmt.Errorf("account_sender_unavailable: the sender registry could not be read")
	}
	var rows []memorynodes.MemoryNode
	if err := db.NewSelect().Model(&rows).Where("concept = ?", concept).
		Where("id IN (?)", bun.In([]string{id, BareShortId(id), concept + ":" + BareShortId(id)})).
		OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx); err != nil {
		return fmt.Errorf("account_sender_unavailable: %w", err)
	}
	if len(rows) == 0 || !organizationAccountsAgree(accountID, stringFromAny(accountRowPayload(rows[0])["accountId"])) {
		return fmt.Errorf("account_sender_mismatch: choose a sender belonging to this account")
	}
	payload["defaultSenderIdentityId"] = concept + ":" + BareShortId(id)
	return nil
}
