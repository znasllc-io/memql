package campaigns

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/memql"
)

// links.go -- the URLs placed in an email (memql#6033).
//
// Every link in a message is built here from the secret and the origin the
// unsubscribe link has always used (Config.UnsubscribeSecret and
// Config.UnsubscribeBaseURL), so there is one place that knows the shape of a
// link and the pages cannot drift from the one-click unsubscribe. Each builder
// mints the token for ITS page and nothing else, so a URL built by ConfirmURL
// opens the confirm page and no other.
//
// A blank origin is refused here (see requireLinkOrigin): a link without one is
// a bare path no mail client can open. Beyond that the builders do not judge the
// origin; a missing or unusable one is refused ahead of a send by
// Config.RequireUnsubscribe, as it is for the unsubscribe link.
//
// # What a link gives away
//
// The confirm, preferences and view tokens are sealed (link_token.go): what they
// carry -- who the message is for, the organization, a digest of the address,
// the names it showed -- cannot be read from the link, and these are the links
// that get forwarded and posted. The unsubscribe token is not: it is the older
// signed-and-readable u3 format, and it has to stay that way, because a link
// already sitting in a mailbox must keep working for years and its format
// cannot change under it. A later change to it needs a version that both
// formats are read through until the old links are gone.
//
// # From builders to a message
//
// The builders above take a payload. A message needs all of its links at once,
// built from the same facts about the same send, so Worker.messageLinks does
// that for the send paths (the campaign drain and the single send) and
// inertLinks does it for the test send. What a message does with them is
// render.go's business: four merge tags, the footer, the List-Unsubscribe pair.

const (
	// ConfirmPath, PreferencesPath and ViewPath are the routes the three
	// email-link pages are served from, beside UnsubscribePath. They are exact
	// paths: the token rides in the query, not in the path.
	ConfirmPath     = "/email/confirm"
	PreferencesPath = "/email/preferences"
	ViewPath        = "/email/view"
)

// UnsubscribeURL is the one-click unsubscribe link for a recipient: the u3
// token at UnsubscribePath.
func UnsubscribeURL(cfg Config, p UnsubscribePayload) (string, error) {
	if err := requireLinkOrigin(cfg); err != nil {
		return "", err
	}
	token, err := MintUnsubscribeToken(cfg.UnsubscribeSecret, p)
	if err != nil {
		return "", err
	}
	return linkURL(cfg.UnsubscribeBaseURL, UnsubscribePath, token), nil
}

// ConfirmURL is the link that opens the confirm-subscription page. p must carry
// ExpiresAt; confirmLinkLifetime is how far ahead a caller sets it.
func ConfirmURL(cfg Config, p LinkPayload) (string, error) {
	return pageURL(cfg, LinkConfirm, ConfirmPath, p)
}

// PreferencesURL is the link that opens the preferences page.
func PreferencesURL(cfg Config, p LinkPayload) (string, error) {
	return pageURL(cfg, LinkPreferences, PreferencesPath, p)
}

// ViewURL is the link that opens the view-in-browser page. p must carry the
// template and the template revision that was sent.
func ViewURL(cfg Config, p LinkPayload) (string, error) {
	return pageURL(cfg, LinkView, ViewPath, p)
}

func pageURL(cfg Config, kind LinkKind, path string, p LinkPayload) (string, error) {
	if err := requireLinkOrigin(cfg); err != nil {
		return "", err
	}
	token, err := MintLinkToken(cfg.UnsubscribeSecret, kind, p)
	if err != nil {
		return "", err
	}
	return linkURL(cfg.UnsubscribeBaseURL, path, token), nil
}

// requireLinkOrigin refuses a blank origin. Without one a link would be a bare
// path (/email/confirm?token=...) that no mail client can open, and nothing
// would say so: the token would seal and the URL would build. An origin of only
// slashes is blank for the same reason.
func requireLinkOrigin(cfg Config) error {
	if strings.TrimRight(strings.TrimSpace(cfg.UnsubscribeBaseURL), "/") == "" {
		return errors.New("campaigns: no unsubscribe base URL configured, so no email link can be built")
	}
	return nil
}

// linkURL joins origin, path and token: <origin><path>?token=<escaped token>.
// Whitespace around the configured origin and any trailing slashes are dropped,
// as validateUnsubscribeBaseURL and the unsubscribe link have always treated
// them; a path inside the origin is kept.
func linkURL(baseURL, path, token string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return base + path + "?token=" + url.QueryEscape(token)
}

// MessageLinks is every address a message can point at. A message points at its
// own unsubscribe page, its preferences page and a copy of itself in a browser;
// a confirmation also points at the page that confirms the subscription.
//
// Unsubscribe is required, and renderMessage refuses a message without it. The
// other three may be empty and then render empty: Confirm is empty on every
// message that is not a confirmation, and View is empty when it could not be
// built (see Worker.messageLinks).
type MessageLinks struct {
	Unsubscribe string
	Preferences string
	View        string
	Confirm     string
}

// exempt is the set of this message's own links that click tracking must leave
// alone. Substituting {{unsubscribeUrl}} and its siblings puts these URLs in the
// body before links are rewritten, and a tracked redirect around a link whose
// whole job is to work on the first click is worse than no tracking: it adds a
// hop to the opt-out, and it counts as engagement a click that is not one.
func (l MessageLinks) exempt() map[string]bool {
	out := make(map[string]bool, 4)
	for _, link := range []string{l.Unsubscribe, l.Preferences, l.View, l.Confirm} {
		if link != "" {
			out[link] = true
		}
	}
	return out
}

// linkRequest is what Worker.messageLinks needs to build one message's links:
// who the message is for, what it is, and what it showed.
//
// The ids may be in either form; messageLinks carries them bare. The campaign is
// empty for a message that belongs to no campaign (a single send made by a
// rule, which is not a persisted campaign and must not be named as one).
type linkRequest struct {
	OwnerUserID string
	RecipientID string
	CampaignID  string
	AccountID   string
	EmailDigest string

	// TemplateID and TemplateRevision say which version of which template was
	// sent. They are the frozen copy's, not the template's current ones: the
	// view page shows the message as it went out.
	TemplateID       string
	TemplateRevision string

	// CampaignName is what {{campaignName}} showed, and SenderName is the name
	// the footer SHOWED -- footerSender's answer, which is the sender's name,
	// else the organization's, else "this mailing list" -- so a later view
	// renders the same words. The stored sender name may be empty and the
	// footer still named somebody; the page has to be given who. A campaign's
	// name is capped where it is saved (maxCampaignNameRunes). A sender's is
	// too, but an organization's name is not, so SenderName is cut to
	// maxSenderNameRunes on its way into the token rather than trusted.
	CampaignName string
	SenderName   string

	// Confirm asks for the confirm-subscription link too. Only a confirmation
	// email is given one: it is a request to start mailing somebody, and it
	// expires confirmLinkLifetime from now.
	Confirm bool
}

// messageLinks builds the links of one message.
//
// Unsubscribe and Preferences are required: a message that cannot be given them
// is not sent, and the error says why. View is not. It is a courtesy, and a
// message that cannot be given one still goes out with the opt-out it has to
// have. It can only fail through a programming error (an id or a revision that
// is missing, or names longer than the caps allow into a token), so the failure
// is logged at error level, loudly enough to be fixed, and the link is left out.
//
// Recipient, campaign and template ids are carried bare, as the unsubscribe token
// has always carried them; the owner is carried as given.
func (w *Worker) messageLinks(req linkRequest) (MessageLinks, error) {
	recipientID, campaignID, templateID := bare(req.RecipientID), bare(req.CampaignID), bare(req.TemplateID)

	var links MessageLinks
	var err error
	if links.Unsubscribe, err = UnsubscribeURL(w.cfg, UnsubscribePayload{
		OwnerUserID: req.OwnerUserID, RecipientID: recipientID, CampaignID: campaignID,
		AccountID: req.AccountID, EmailDigest: req.EmailDigest,
	}); err != nil {
		return MessageLinks{}, err
	}

	// The campaign rides every page's token, not only the view page's: it is
	// what an opt-out or a consent decision made on a page is attributed to, as
	// the unsubscribe link's has always been.
	page := LinkPayload{
		OwnerUserID: req.OwnerUserID, RecipientID: recipientID, CampaignID: campaignID,
		AccountID: req.AccountID, EmailDigest: req.EmailDigest,
	}
	if links.Preferences, err = PreferencesURL(w.cfg, page); err != nil {
		return MessageLinks{}, err
	}

	view := page
	view.TemplateID, view.TemplateRevision = templateID, req.TemplateRevision
	view.SenderName, view.CampaignName = clipRunes(req.SenderName, maxSenderNameRunes), req.CampaignName
	if links.View, err = ViewURL(w.cfg, view); err != nil {
		w.logger.Error("campaigns: no view-in-browser link could be built; the message goes without it",
			"campaign", campaignID, "template", templateID, "recipient", recipientID, "error", err)
		links.View = ""
	}

	if req.Confirm {
		confirm := page
		confirm.ExpiresAt = w.nowUTC().Add(confirmLinkLifetime).Unix()
		if links.Confirm, err = ConfirmURL(w.cfg, confirm); err != nil {
			return MessageLinks{}, err
		}
	}
	return links, nil
}

// inertLinks is the four links of a test send. Every one carries
// inertUnsubscribeToken, which is signed by nothing, so the operator sees what
// the recipient will see and a click does nothing: a real token would name a
// real recipient.
func inertLinks(cfg Config) MessageLinks {
	return MessageLinks{
		Unsubscribe: linkURL(cfg.UnsubscribeBaseURL, UnsubscribePath, inertUnsubscribeToken),
		Preferences: linkURL(cfg.UnsubscribeBaseURL, PreferencesPath, inertUnsubscribeToken),
		View:        linkURL(cfg.UnsubscribeBaseURL, ViewPath, inertUnsubscribeToken),
		Confirm:     linkURL(cfg.UnsubscribeBaseURL, ConfirmPath, inertUnsubscribeToken),
	}
}

// The longest names the view link has to carry.
//
// The view link carries the campaign's name and the sender's, and a token over
// maxLinkTokenBytes is refused. Both are free text an operator types, so the
// bound has to come from where they are saved (the sender's name the link
// carries can also be the organization's, which has no cap of its own, and is
// cut here instead; see linkRequest). The numbers are the engine's
// (memql.CampaignNameMaxRunes and SenderNameMaxRunes, whose write guard refuses a
// longer name with a sentence for the person who typed it), and
// dsl/campaigns/concepts.memql declares the same on campaign.name,
// campaign.fromName and senderIdentity.fromName; a test ties each declaration to
// these constants. At these lengths the longest token possible is about 3 kB,
// with every character one that JSON writes in six bytes.
const (
	maxCampaignNameRunes = memql.CampaignNameMaxRunes
	maxSenderNameRunes   = memql.SenderNameMaxRunes
)

// clipRunes cuts s to at most limit characters, never in the middle of one.
func clipRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit])
}

// nameWithSuffix is a campaign name made from another's: a recurring campaign's
// occurrence ("<name> · 2026-10-12 09:30 UTC") or a test run ("<name> · Test").
// The result fits maxCampaignNameRunes whatever the base, because the engine
// refuses a campaign saved over it. It is the base that gives way and never the
// suffix, which is what says which occurrence this is.
func nameWithSuffix(base, suffix string) string {
	return clipRunes(base, maxCampaignNameRunes-utf8.RuneCountInString(suffix)) + suffix
}
