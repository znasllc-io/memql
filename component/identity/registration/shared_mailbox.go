package registration

import "strings"

// shared_mailbox.go -- the local-part heuristic behind the shared-mailbox
// hint (memql#4304).
//
// # What the hint is for
//
// A MemQL account registered as `team@example.com` is a shared account, and
// nothing about it says so. Anyone who can read that mailbox can request a
// sign-in link and enter the account -- so the account's real sign-in
// surface is the mailbox's reader list, which is a fact nobody can see from
// inside the product. Device binding (memql#4302) stops a colleague riding
// somebody else's link; it does nothing about a colleague requesting their
// own. What closes that is `signInPolicy=passkey_only`, and what makes
// anybody think to set it is this flag.
//
// # It is a HINT, and that is the design
//
// The alternative -- refusing group aliases at registration -- was
// considered and rejected (design D1). It has false positives (`info@` for a
// solo operator), it says nothing about the accounts that already exist, and
// it is trivially defeated by a mailbox called `sales-team-uk`. So the
// heuristic never blocks: it sets a boolean, the boolean drives copy, and
// the user or an admin can clear it in one click.
//
// # The list is pinned by a test
//
// Not because the exact membership is load-bearing -- it plainly is not --
// but because a silent edit changes what the product tells people about
// their own security posture, and that deserves to be a visible diff.

// rfc2142RoleNames is the RFC 2142 mailbox set: addresses an organisation is
// EXPECTED to run, and which are therefore almost never one person's.
var rfc2142RoleNames = []string{
	"abuse",
	"hostmaster",
	"info",
	"marketing",
	"noc",
	"postmaster",
	"sales",
	"security",
	"support",
	"webmaster",
	"www",
}

// commonTeamNames is the part RFC 2142 does not cover: the names
// organisations actually pick for a shared inbox.
var commonTeamNames = []string{
	"admin",
	"billing",
	"contact",
	"dev",
	"finance",
	"hello",
	"hr",
	"it",
	"legal",
	"no-reply",
	"noreply",
	"office",
	"ops",
	"team",
}

// SharedMailboxLocalParts returns every local part the heuristic matches, in
// one slice, for the test that pins it and for anything that wants to render
// the list.
func SharedMailboxLocalParts() []string {
	out := make([]string, 0, len(rfc2142RoleNames)+len(commonTeamNames))
	out = append(out, rfc2142RoleNames...)
	out = append(out, commonTeamNames...)
	return out
}

// LooksLikeSharedMailbox reports whether an address's local part is one of
// the known role or team names.
//
// EXACT MATCH ON THE LOCAL PART, not a substring or prefix test, and that is
// deliberate. Substring matching would flag `itzhak@`, `devon@`,
// `winfo@` and `sales.rodriguez@` -- real people, told by their own product
// that their personal account looks like a shared mailbox. A hint that is
// wrong about individuals is worse than a hint that misses some team
// inboxes, because the miss costs nothing and the false positive costs
// trust in every other thing the page says.
//
// A `+tag` suffix is stripped first (`support+eu@` is still support@), and
// the comparison folds case. Anything that is not an address returns false.
func LooksLikeSharedMailbox(email string) bool {
	local := normalizedLocalPart(email)
	if local == "" {
		return false
	}
	for _, name := range rfc2142RoleNames {
		if local == name {
			return true
		}
	}
	for _, name := range commonTeamNames {
		if local == name {
			return true
		}
	}
	return false
}

// normalizedLocalPart lowercases the local part and drops any `+tag`.
func normalizedLocalPart(email string) string {
	email = strings.TrimSpace(strings.ToLower(email))
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	local := email[:at]
	if plus := strings.IndexByte(local, '+'); plus >= 0 {
		local = local[:plus]
	}
	return strings.TrimSpace(local)
}
