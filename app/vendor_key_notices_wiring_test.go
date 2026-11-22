package app

// vendor_key_notices_wiring_test.go -- guards the wiring of the notice a key's
// owner gets when it stops working (design record
// 2026-11-21-vendor-accounts-and-api-keys, decision K13).
//
// WHY A SOURCE-LEVEL GUARD, as campaign_digest_sweep_wiring_test.go: nothing in
// the repo calls app.Build(), and standing up a real App needs a database. The
// behaviour is tested where it lives: who is owed a notice and that it is one
// per fall in integrations/vendorkeys, the words in
// component/identity/emailsender, the schedule in component/automations. What
// only this package can lose is the CONNECTION between them. Delete the call
// and every one of those suites still passes while the pass finds no notifier
// on any node, counts every notice as waiting, and no owner is ever told.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/emailsender"
	"github.com/znasllc-io/memql/integrations/vendorkeys"
)

func TestTheKeyNoticeSenderIsInstalledOnEveryNode(t *testing.T) {
	src, err := os.ReadFile("integrations.go")
	if err != nil {
		t.Fatalf("read integrations.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (a *App) integrationsCore() {")
	if start < 0 {
		t.Fatal("could not find integrationsCore in integrations.go; this guard needs updating")
	}
	core := body[start:]
	if end := strings.Index(core, "\n}\n"); end >= 0 {
		core = core[:end]
	}

	// integrationsCore runs on every node type, and the pass is placed on a
	// bff by its automation, not by a build tag here.
	const install = "a.wireVendorKeyNotices()"
	installAt := strings.Index(core, install)
	if installAt < 0 {
		t.Fatal("integrationsCore no longer installs what mails a key's owner. Without it the pass that tells owners finds no notifier, " +
			"counts every notice as waiting, and nobody learns that their API key stopped working. Restore " + install + ".")
	}
	// The plug-in is built by materializePlugins: a notifier looked up before
	// that finds no integration to install it on.
	materializeAt := strings.Index(core, "a.materializePlugins()")
	if materializeAt < 0 {
		t.Fatal("could not find a.materializePlugins() in integrationsCore; this guard needs updating")
	}
	if installAt < materializeAt {
		t.Error("the notifier must be installed AFTER the plug-ins are materialized: before that there is no key integration to install it on")
	}

	method, err := os.ReadFile("vendor_key_notices.go")
	if err != nil {
		t.Fatalf("read vendor_key_notices.go: %v", err)
	}
	for _, want := range []string{
		`a.engine.IntegrationByName("vendorkeys").(*vendorkeys.Integration)`,
		"integration.SetNotifier(a.sendVendorKeyNotice)",
		".SendVendorKeyNotice(ctx, vendorKeyNoticeMail(notice, cfg.BrandName))",
	} {
		if !strings.Contains(string(method), want) {
			t.Errorf("vendor_key_notices.go no longer contains %q", want)
		}
	}
}

// A node with no engine has nothing to resolve a mail sender off. It says so,
// and the pass that asked keeps the notice owed.
func TestAKeyNoticeWithNoEngineIsNotReportedAsSent(t *testing.T) {
	a := &App{}
	if err := a.sendVendorKeyNotice(context.Background(), vendorkeys.KeyNotice{OwnerEmail: "dana@acme.test"}); err == nil {
		t.Fatal("a notice with no engine to send it through was reported as sent")
	}
}

// The mail says what the pass found, field for field, under the cluster's
// name. The key's id and its owner's id stay behind: the mail has no use for
// either.
func TestAKeyNoticeIsMailedAsItWasFound(t *testing.T) {
	when := time.Date(2026, 11, 22, 14, 30, 0, 0, time.UTC)
	got := vendorKeyNoticeMail(vendorkeys.KeyNotice{
		KeyId: "k1", OwnerUserId: "u1", OwnerEmail: "dana@acme.test", OwnerName: "Dana",
		Label: "Personal", Vendor: "openai", State: "credit", Detail: "out of credit", At: when,
	}, "Acme")
	want := emailsender.VendorKeyNotice{
		Email: "dana@acme.test", OwnerName: "Dana", KeyLabel: "Personal",
		Vendor: "openai", State: "credit", Detail: "out of credit", At: when, BrandName: "Acme",
	}
	if got != want {
		t.Errorf("the mail = %+v, want %+v", got, want)
	}
}
