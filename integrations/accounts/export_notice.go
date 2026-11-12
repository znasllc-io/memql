package accounts

// export_notice.go -- the email every active owner gets when an account export
// is started (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section H, "Owners
// are mailed").
//
// It names the account, who started the export and when, and nothing else: no
// passphrase, no file name, and no link of any kind. A link that acts on an
// export, mailed to an address others may read, would be a handle on the
// export for whoever reads it; the copy says to open MemQL OS instead.
//
// It goes out the way identity's own notices do: the engine's email
// integration, resolved off the integration registry at SEND time (on a booting
// node the registry may not be populated yet), from the deployment's one
// configured mailbox (the zero SendAs). The recipients are read through
// identity's `activeUsers(role: "owner")`, as the access-request notice reads
// them.
//
// It is sent after the start is committed and off the request
// (Exports.mailOwners), under one thirty-second bound for the whole notice, so
// a slow or hanging mail provider never holds a person's start. A failure to
// send is logged and does not fail the start: a cluster that cannot send mail
// still exports, and the audit trail holds the record.
//
// Tests: export_db_test.go, TestExportStartMailsEveryOwnerWithNoPassphraseAndNoLink.

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/email"
)

// newOwnerNotice builds Exports.notifyOwners from where the owners' addresses
// come from and how mail is sent.
func newOwnerNotice(owners func(context.Context) ([]string, error), sender func() email.Sender, logger *slog.Logger) func(context.Context, Export, string) {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, x Export, startedBy string) {
		// ctx is the bounded, detached context Exports.mailOwners gives it.
		addresses, err := owners(ctx)
		if err != nil {
			logger.Warn("account export: the owners could not be read, so nobody was mailed", "exportId", x.ID, "error", err)
			return
		}
		var s email.Sender
		if sender != nil {
			s = sender()
		}
		if s == nil {
			logger.Warn("account export: this node has no email sender, so the owners were not mailed", "exportId", x.ID, "owners", len(addresses))
			return
		}
		// A failure is logged once, with the export and how many addresses
		// failed, and never with an address: the log store keeps what is
		// logged here. A provider's error may quote the address it refused,
		// so each address is taken out of the error text too.
		msg := exportNoticeMessage(x, startedBy)
		failed := 0
		var last error
		for _, to := range addresses {
			msg.To = to
			if err := s.Send(ctx, msg, email.SendAs{}); err != nil {
				failed++
				last = err
			}
		}
		if failed > 0 {
			text := last.Error()
			for _, a := range addresses {
				text = strings.ReplaceAll(text, a, "[address]")
			}
			logger.Warn("account export: owners could not be mailed", "exportId", x.ID, "failed", failed, "owners", len(addresses), "error", text)
		}
	}
}

// exportNoticeMessage is the message without its recipient.
func exportNoticeMessage(x Export, startedBy string) email.Message {
	account := x.AccountName
	if strings.TrimSpace(account) == "" {
		account = x.AccountID
	}
	if strings.TrimSpace(startedBy) == "" {
		startedBy = x.RequestedBy
	}
	when := time.Now().UTC().Format("2 January 2006 at 15:04 UTC")
	text := fmt.Sprintf("An export of the account %s (%s) was started by %s on %s.\n\n"+
		"The export holds everything the account holds. If you did not expect it, open MemQL OS, then Accounts > %s > Export, where it can be cancelled.\n",
		account, x.AccountID, startedBy, when, account)
	html := fmt.Sprintf("<p>An export of the account <strong>%s</strong> (%s) was started by %s on %s.</p>"+
		"<p>The export holds everything the account holds. If you did not expect it, open MemQL OS, then Accounts &gt; %s &gt; Export, where it can be cancelled.</p>",
		template.HTMLEscapeString(account), template.HTMLEscapeString(x.AccountID), template.HTMLEscapeString(startedBy),
		template.HTMLEscapeString(when), template.HTMLEscapeString(account))
	return email.Message{
		Subject:  fmt.Sprintf("An export of %s was started", account),
		TextBody: text,
		HTMLBody: html,
	}
}

// activeOwnerEmails reads every active owner's address through identity's own
// query, under internal origin because the query is @serverOnly.
func activeOwnerEmails(engine Executor) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		if engine == nil {
			return nil, fmt.Errorf("no engine to read the owners with")
		}
		res, err := engine.Execute(auth.ContextWithInternalOrigin(ctx), `query activeUsers(role: "owner")`)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, nil
		}
		seen := map[string]bool{}
		var out []string
		add := func(address string) {
			address = strings.TrimSpace(address)
			if address != "" && !seen[strings.ToLower(address)] {
				seen[strings.ToLower(address)] = true
				out = append(out, address)
			}
		}
		if res.Bundle != nil && len(res.Bundle.Nodes) > 0 {
			for _, n := range res.Bundle.Nodes {
				if n.GetPayload() != nil {
					add(n.GetPayload().GetFields()["primaryEmail"].GetStringValue())
				}
			}
			return out, nil
		}
		_, data, err := res.ToAPIResult()
		if err != nil {
			return nil, err
		}
		for _, v := range data {
			if sv := v.GetStructValue(); sv != nil {
				add(sv.GetFields()["primaryEmail"].GetStringValue())
			}
		}
		return out, nil
	}
}
