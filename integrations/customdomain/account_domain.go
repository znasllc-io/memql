package customdomain

// account_domain.go -- the ACCOUNT's domain walk (epic memql#5165, section F).
//
// A client's own domain becomes three things, in this order of arrival:
// ownership proven by a TXT token, then domain join (a person with a verified
// address on that domain lands in the account's group), then a reserved MemQL
// name. Nothing below ownership is legal before it.
//
// # Why it lives here rather than in integrations/groups
//
// Because the ownership check is already here. `CheckOwnership` at
// `_memql-verify.<domain>`, the token mint, the resolver and the two-minute
// schedule are the custom-domain reconciler's, and an account's domain asks
// exactly the same question of DNS that a site's custom domain does. A second
// implementation would be a second thing to get right about a security check,
// and the two would drift on the day somebody fixed one of them.
//
// What is NOT shared is the row and its states: v1:platform:customDomain walks
// pending -> verifying -> issuing -> live because a certificate follows, and an
// account's domain stops at `verified` because nothing is served from it yet.
//
// # One state per pass, and verified accounts cost nothing
//
// The walk reads only accounts whose domain is set and not yet verified, so a
// cluster whose clients are all verified does one read per tick and no
// lookups. That is the reconciler's own discipline: the row is the state
// machine, and letting the next pass pick a row up is the only coordination
// two replicas need.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// The account domain states. Written out because the walk, the guards in
// component/memql and the OS all compare against them.
const (
	AccountDomainUnverified = "unverified"
	AccountDomainVerifying  = "verifying"
	AccountDomainVerified   = "verified"
)

// AccountDomainPass is what one sweep did.
type AccountDomainPass struct {
	Checked  int `json:"checked"`
	Minted   int `json:"minted"`
	Verified int `json:"verified"`
	Failed   int `json:"failed"`
	Reserved int `json:"reserved"`
}

// accountRow is the slice of v1:accounts:account this walk reads.
type accountRow struct {
	ID          string
	Domain      string
	Token       string
	Status      string
	MemqlDomain string
	Reserved    string
}

// ReconcileAccountDomains walks every active account whose domain is set and
// not yet verified.
func (i *Integration) ReconcileAccountDomains(ctx context.Context) (AccountDomainPass, error) {
	var out AccountDomainPass
	rows, err := i.accountsToWalk(ctx)
	if err != nil {
		return out, err
	}
	selected := map[string]accountRow{}
	ids := []string{}
	for _, row := range rows {
		selected[row.ID] = row
		ids = append(ids, row.ID)
	}
	_, err = workflowhost.Run(ctx, "accountDomainReconcileWorkflow", map[string]any{"ids": ids}, workflowhost.Options{Logger: i.logger, Operations: map[string]workflowhost.Operation{
		"customDomainReconcileAccount": func(ctx context.Context, a map[string]any) (any, error) {
			id, _ := a["accountId"].(string)
			row, ok := selected[id]
			if !ok {
				return nil, fmt.Errorf("account outside reconciliation snapshot")
			}
			out.Checked++
			err := i.stepAccountDomain(ctx, row, &out)
			if err != nil {
				out.Failed++
				if i.logger != nil {
					i.logger.Warn("account domain reconciliation step failed", "account", row.ID, "error", err)
				}
			}
			return nil, err
		},
	}})
	return out, err
}

// stepAccountDomain advances ONE account by at most one state.
func (i *Integration) stepAccountDomain(ctx context.Context, row accountRow, out *AccountDomainPass) error {
	now := time.Now().UTC()

	// No token yet: mint one and stop. The person has to publish it before
	// a lookup can find it, so checking in the same pass would spend a DNS
	// query to learn something already known.
	if strings.TrimSpace(row.Token) == "" {
		token, err := mintToken()
		if err != nil {
			return err
		}
		out.Minted++
		return i.writeAccountDomain(ctx, row.ID, map[string]string{
			"domainToken":         token,
			"domainStatus":        AccountDomainVerifying,
			"domainLastCheckedAt": stampTime(now),
		})
	}

	if res := CheckOwnership(ctx, i.resolver, row.Domain, row.Token); !res.OK {
		// A FAILURE IS NOT AN ERROR. The record is not published yet, or
		// is wrong -- both are states the person fixes at their registrar,
		// and both are reported on the row rather than raised. Raising
		// would put a warning in the log every two minutes for every
		// client who has not finished pasting a record.
		return i.writeAccountDomain(ctx, row.ID, map[string]string{
			"domainStatus":        AccountDomainVerifying,
			"domainFailureReason": res.Reason,
			"domainFailureDetail": res.Detail,
			"domainLastCheckedAt": stampTime(now),
		})
	}

	fields := map[string]string{
		"domainStatus":        AccountDomainVerified,
		"domainVerifiedAt":    stampTime(now),
		"domainLastCheckedAt": stampTime(now),
		"domainFailureReason": "",
		"domainFailureDetail": "",
	}
	// The reservation is stamped in the SAME write, and only when the name
	// passes the guardrails. A separate write would leave a window where
	// the account is verified and its name unreserved, which is exactly the
	// window a second account could claim it in.
	if name := strings.TrimSpace(row.MemqlDomain); name != "" && strings.TrimSpace(row.Reserved) == "" {
		fields["memqlReservedAt"] = stampTime(now)
		out.Reserved++
	}
	out.Verified++
	if i.logger != nil {
		i.logger.Info("account domain verified", "account", row.ID, "domain", row.Domain)
	}
	return i.writeAccountDomain(ctx, row.ID, fields)
}

// accountsToWalk reads the accounts with work to do.
func (i *Integration) accountsToWalk(ctx context.Context) ([]accountRow, error) {
	// The stamp is INLINE rather than left to SystemActorContext, which also
	// applies it. Both reads here name @serverOnly constructs, and a stamp
	// hidden inside a helper is one a static check cannot see -- so the
	// requirement is written where the call is, which is what
	// component/auth/call_origin.go asks for and what
	// TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin reads.
	res, err := i.store.engine.Execute(auth.ContextWithInternalOrigin(SystemActorContext(ctx)), "query accountsForDomainWalk()")
	if err != nil {
		return nil, fmt.Errorf("customdomain: accountsForDomainWalk: %w", err)
	}
	rows := memql.MaterializeRows(res)
	out := make([]accountRow, 0, len(rows))
	for _, r := range rows {
		row := accountRow{
			ID:          memql.BareShortId(rowString(r, "id")),
			Domain:      NormalizeHostname(rowString(r, "domain")),
			Token:       rowString(r, "domainToken"),
			Status:      rowString(r, "domainStatus"),
			MemqlDomain: rowString(r, "memqlDomain"),
			Reserved:    rowString(r, "memqlReservedAt"),
		}
		// Stated in Go as a guard rather than trusted from the filter, the
		// reconciler's own discipline: a future widening of that query must
		// not silently start spending DNS lookups on rows that are done.
		if row.Domain == "" || row.Status == AccountDomainVerified {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// writeAccountDomain writes the walk's fields onto one account.
func (i *Integration) writeAccountDomain(ctx context.Context, accountID string, fields map[string]string) error {
	var q strings.Builder
	q.WriteString("mutation recordAccountDomainCheck(accountId: ")
	q.WriteString(langparser.QuoteString(accountID))
	for _, key := range accountDomainWriteOrder {
		value, present := fields[key]
		if !present {
			continue
		}
		q.WriteString(", ")
		q.WriteString(key)
		q.WriteString(": ")
		q.WriteString(langparser.QuoteString(value))
	}
	q.WriteString(")")
	if _, err := i.store.engine.Execute(auth.ContextWithInternalOrigin(SystemActorContext(ctx)), q.String()); err != nil {
		return fmt.Errorf("customdomain: recordAccountDomainCheck: %w", err)
	}
	return nil
}

// accountDomainWriteOrder fixes the argument order so a rendered statement is
// byte-stable for a given field set -- which is what lets a test assert one.
var accountDomainWriteOrder = []string{
	"domainToken",
	"domainStatus",
	"domainFailureReason",
	"domainFailureDetail",
	"domainLastCheckedAt",
	"domainVerifiedAt",
	"memqlReservedAt",
}

func stampTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
