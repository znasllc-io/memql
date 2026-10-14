package memql

import (
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"strings"
	"testing"
)

func TestAccountCreationDefaultsPersistAcrossReplicas(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("domain-defaults")
	ownerID, account, other, sender := "owner-"+suffix, "account-"+suffix, "other-"+suffix, "sender-"+suffix
	seedPrincipal(t, engine, ownerID, auth.RoleOwner)
	owner := rankActorCtx(ownerID, auth.RoleOwner)
	for _, id := range []string{account, other} {
		runMutation(t, owner, engine, "createClientAccount", map[string]any{"accountId": id, "name": id, "domain": "profile.example.test"})
	}
	if err := organizationInsert(t, engine, owner, "v1:campaigns:senderIdentity", sender, map[string]any{"ownerUserId": ownerID, "accountId": account, "address": "news@example.test", "fromName": "News", "status": "active"}); err != nil {
		t.Fatal(err)
	}
	runMutation(t, owner, engine, "configureAccountDomains", map[string]any{"accountId": account, "domains": []string{" EXAMPLE.TEST. ", "mail.example.test"}, "defaultDomain": "EXAMPLE.TEST", "defaultSenderIdentityId": sender})
	receiver, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	receiver.Logger = engine.Logger
	if err := receiver.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	// This receiver holds no browser session or originating request state.
	remote := rankActorCtx(ownerID, auth.RoleOwner)
	row := readOneAccount(t, remote, receiver, account)
	if row["defaultDomain"] != "example.test" || BareShortId(stringFromAny(row["defaultSenderIdentityId"])) != sender || row["domain"] != "profile.example.test" || row["joinOnDomain"] == true {
		t.Fatalf("wrong defaults or access policy after hop: %#v", row)
	}
	// The field is visible through the collection shape, not just the detail query.
	result, err := receiver.Execute(remote, `query clientAccountsAll(includeArchived: true)`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, value := range MaterializeRows(result) {
		if BareShortId(stringFromAny(value["id"])) == account {
			found = value["defaultDomain"] == "example.test"
		}
	}
	if !found {
		t.Fatal("account options omitted defaults")
	}
	for _, statement := range []string{
		fmt.Sprintf(`mutation configureAccountDomains(accountId:%s, domains:["other.test"], defaultDomain:"other.test", defaultSenderIdentityId:%s)`, langparser.QuoteString(other), langparser.QuoteString(sender)),
		fmt.Sprintf(`mutation configureAccountDomains(accountId:%s, domains:["example.test"], defaultDomain:"unassigned.test")`, langparser.QuoteString(account)),
	} {
		if _, err := receiver.Execute(remote, statement); err == nil {
			t.Fatalf("accepted invalid defaults: %s", statement)
		}
	}
	if err := organizationInsert(t, receiver, remote, "v1:campaigns:senderIdentity", sender, map[string]any{"accountId": other, "ownerUserId": ownerID, "address": "news@example.test", "fromName": "News", "status": "active"}); err == nil || !strings.Contains(err.Error(), "organization_transfer_refused") {
		t.Fatalf("moved a sender still used as an account default: %v", err)
	}
	member := "member-" + suffix
	seedPrincipal(t, engine, member, auth.RoleWriter)
	if _, err := receiver.Execute(rankActorCtx(member, auth.RoleWriter), fmt.Sprintf(`mutation configureAccountDomains(accountId:%s, domains:[], defaultDomain:"")`, langparser.QuoteString(account))); err == nil {
		t.Fatal("unprivileged caller changed account defaults")
	}
	if err := organizationInsert(t, receiver, remote, conceptAccountsAccount, account, map[string]any{"domains": []string{"https://bad.test"}, "defaultDomain": "https://bad.test"}); err == nil || !strings.Contains(err.Error(), "account_domain_invalid") {
		t.Fatalf("raw write escaped domain invariant: %v", err)
	}
	runMutation(t, remote, receiver, "configureAccountDomains", map[string]any{"accountId": account, "domains": []string{}, "defaultDomain": "", "defaultSenderIdentityId": ""})
	row = readOneAccount(t, remote, receiver, account)
	if row["defaultDomain"] != "" || row["defaultSenderIdentityId"] != "" {
		t.Fatalf("opt-out did not persist: %#v", row)
	}
}
