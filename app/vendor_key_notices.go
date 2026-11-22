package app

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/emailsender"
	"github.com/znasllc-io/memql/integrations/vendorkeys"
)

// vendor_key_notices.go -- hands the key integration what mails a key's owner
// when the key stops working (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K13).
//
// # Why it is wired here
//
// integrations/vendorkeys decides who is owed a notice and that it is one per
// fall; component/identity/emailsender holds the words and the one path this
// repository sends identity mail through. Neither may import the other: the
// integration takes a function, and this is where a mail sender can be
// reached. It is the meeting point sendInvitationEmail is (transport.go), for
// the same reason.
//
// # On every node type, though one bff sends
//
// The pass that calls the notifier is placed on one bff replica by its
// automation (dsl/router/automations.memql). Installing the notifier
// everywhere costs a function pointer and means a placement changed in the
// DSL does not silently land the pass on a node that can tell nobody.
//
// Regression tests: vendor_key_notices_wiring_test.go.

// wireVendorKeyNotices installs the notifier on the REGISTERED key
// integration, never on a second one: capability dispatch goes through the
// instance the registry built.
func (a *App) wireVendorKeyNotices() {
	integration, ok := a.engine.IntegrationByName("vendorkeys").(*vendorkeys.Integration)
	if !ok {
		// A test engine or a stripped build. Debug, not a warning.
		if a.Logger != nil {
			a.Logger.Debug("API key notices not wired: the vendorkeys plug-in did not materialize")
		}
		return
	}
	integration.SetNotifier(a.sendVendorKeyNotice)
}

// sendVendorKeyNotice mails one key's owner through the shim the identity
// service sends every message through (see sendInvitationEmail for why there
// is no second path). An error means nothing was sent: the pass keeps the
// notice owed and tries again later.
//
// The shim is built per call so the brand is the one in the cluster settings
// row now, not the one at boot.
func (a *App) sendVendorKeyNotice(ctx context.Context, notice vendorkeys.KeyNotice) error {
	if a == nil || a.engine == nil {
		return fmt.Errorf("vendor keys: no engine on this node, so no API key notice could be sent")
	}
	// Brand, from the settings row when it answers. Absent is not an error:
	// emailsender defaults the name to "MemQL".
	var cfg identity.Config
	store := &identity.Store{Engine: a.engine, Logger: a.Logger}
	if row, err := store.ReadClusterSettings(ctx); err == nil && row != nil {
		cfg.BrandName = row.BrandName
	}
	return emailsender.New(a.engine, a.Logger, cfg).SendVendorKeyNotice(ctx, vendorKeyNoticeMail(notice, cfg.BrandName))
}

// vendorKeyNoticeMail is the mail for one notice. The key's id and its
// owner's id are left behind: the message has no use for either.
func vendorKeyNoticeMail(notice vendorkeys.KeyNotice, brand string) emailsender.VendorKeyNotice {
	return emailsender.VendorKeyNotice{
		Email:     notice.OwnerEmail,
		OwnerName: notice.OwnerName,
		KeyLabel:  notice.Label,
		Vendor:    notice.Vendor,
		State:     notice.State,
		Detail:    notice.Detail,
		At:        notice.At,
		BrandName: brand,
	}
}
