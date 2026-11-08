package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/compose/emaildoc"
	"github.com/znasllc-io/memql/integrations/email"
)

// email_page_brand.go -- how the pages behind email links look and who they
// name.
//
// A person reaches these pages from a link in an email, and they should look
// like it came from the organization that sent it: its name in the tab, its logo
// and its colour on the button, and somebody to write to when the page cannot
// help -- the address the organization's own email connection names. All of it
// comes from the organization the SIGNED token names. A token that did not verify
// names nobody, so a forged link cannot choose which organization a page speaks
// for.
//
// # What a brand is trusted with
//
// Brand values are operator input shown on a public page, so each is checked
// where it is read: a colour only in the two hex forms, a logo only as the
// kit's own saved picture or an absolute https address, everything escaped. The page never fails for want of a
// brand: an opt-out page has to work whatever state the kit is in, so a kit that
// does not read renders the page plain and says so in the log.

// pageBrand is what one page borrows from its organization. Every field may be
// empty; the zero value is the plain page.
type pageBrand struct {
	// Org is the organization's name.
	Org string
	// Color is the primary colour, "#rgb" or "#rrggbb", used for the button.
	Color string
	// Logo is the organization's logo: its saved data: picture or an absolute
	// https address (see logoSource).
	Logo string
	// contact finds the address of somebody who can help: the one the
	// organization's own email connection names (its reply address, else its
	// default sender). It is asked only by a page that names a contact
	// (contactLine) -- most pages do not, and the answer comes from the email
	// transport. Nil, or an empty answer, names nobody.
	contact func() string
}

// contactLookup is where a page's contact comes from: the address the
// organization's own email connection names, through the email transport.
type contactLookup func(ctx context.Context, accountID string) (string, error)

// transportContact is the contact lookup of the email transport resolve returns
// when it is asked (email.Contact): the organization's own connection, resolved
// as the organization's sender is, and never the installation's own mailbox.
func transportContact(resolve func() email.Sender) contactLookup {
	if resolve == nil {
		return nil
	}
	return func(ctx context.Context, accountID string) (string, error) {
		return email.Contact(ctx, resolve(), accountID)
	}
}

var hexColour = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// title is a page title: the generic one, or the one that names the
// organization ("%s" is where its name goes).
func (b pageBrand) title(generic, named string) string {
	if b.Org == "" {
		return generic
	}
	return fmt.Sprintf(named, b.Org)
}

// contactLine is the sentence a page that cannot help ends with.
func (b pageBrand) contactLine() string {
	contact := ""
	if b.contact != nil {
		contact = strings.TrimSpace(b.contact())
	}
	if contact == "" {
		return "<p>You can reply to the email this link came from.</p>"
	}
	return "<p>You can write to " + html.EscapeString(contact) + " for help.</p>"
}

// luminance is the WCAG relative luminance of a "#rgb" or "#rrggbb" colour, or
// -1 when it does not parse.
func luminance(colour string) float64 {
	hex := strings.TrimPrefix(colour, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) != 6 {
		return -1
	}
	var lum float64
	for i, weight := range []float64{0.2126, 0.7152, 0.0722} {
		v, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return -1
		}
		c := float64(v) / 255
		if c <= 0.03928 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		lum += weight * c
	}
	return lum
}

// buttonText is the text colour on the primary colour: whichever of white and
// near-black has the higher contrast ratio against it, so the button stays
// readable whatever the brand picked.
func buttonText(colour string) string {
	lum := luminance(colour)
	if lum < 0 {
		return "#fff"
	}
	white := 1.05 / (lum + 0.05)
	dark := (lum + 0.05) / (luminance("#111") + 0.05)
	if dark > white {
		return "#111"
	}
	return "#fff"
}

// logoPicture matches the pictures a saved kit holds: the media types
// emaildoc.ParseBrandKit accepts for a logo (png, jpeg, gif) as base64, nothing
// else. It is checked again at render because the page does not trust what the
// kit row says it validated.
var logoPicture = regexp.MustCompile(`^data:image/(?:png|jpeg|gif);base64,[A-Za-z0-9+/]+=*$`)

// logoSource is the logo a brand kit's logo may be shown from, or "": the data:
// picture a kit is saved with, or an absolute https address. Anything else
// (another scheme, another media type, a quote or space that could leave the
// attribute) is dropped. The page's policy lets images load from data: and
// https: and nowhere else.
func logoSource(src string) string {
	src = strings.TrimSpace(src)
	if logoPicture.MatchString(src) && len(src) <= emaildoc.MaxBytes {
		return src
	}
	if !strings.HasPrefix(src, "https://") || len(src) <= len("https://") || strings.ContainsAny(src, " \t\r\n\"'<>") {
		return ""
	}
	return src
}

// brandFor reads the organization's brand for a page. ctx reads as the system
// (EmailLinkHandler.organizationRead says why). org is the organization's name,
// already read by the caller (empty when it did not resolve). contact is where the page's contact comes from, asked only
// if the page names one; nil names nobody. A read that fails costs the page that
// part of its brand and is logged at warn; it never fails the page.
func (s *Store) brandFor(ctx context.Context, logger *slog.Logger, accountID, org string, contact contactLookup) pageBrand {
	brand := pageBrand{Org: strings.TrimSpace(org)}
	if strings.TrimSpace(accountID) == "" {
		return brand
	}
	row, found, err := s.BrandKitForAccount(ctx, accountID)
	if err != nil {
		logger.Warn("campaigns: an email page could not read its organization's brand kit; the page is shown without it", "account", bare(accountID), "error", err)
	} else if found {
		raw, err := plainJSON(row["kit"])
		var kit struct {
			Theme struct {
				Primary string `json:"primary"`
			} `json:"theme"`
			Logo struct {
				Src string `json:"src"`
			} `json:"logo"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &kit)
		}
		if err != nil {
			logger.Warn("campaigns: an email page could not read its organization's brand kit; the page is shown without it", "account", bare(accountID), "error", err)
		} else {
			if hexColour.MatchString(kit.Theme.Primary) {
				brand.Color = kit.Theme.Primary
			}
			brand.Logo = logoSource(kit.Logo.Src)
		}
	}
	if contact != nil {
		brand.contact = func() string {
			// Its own bound: the brand's reads may have had theirs already, and a
			// slow connection costs the page its contact, never its answer.
			read, cancel := context.WithTimeout(context.WithoutCancel(ctx), brandReadTimeout)
			defer cancel()
			address, err := contact(read, bare(accountID))
			if err != nil {
				logger.Warn("campaigns: an email page could not read its organization's email connection for a contact; the page says to reply to the email", "account", bare(accountID), "error", err)
				return ""
			}
			return address
		}
	}
	return brand
}

// pageBrandFor is the brand of the organization a verified token names: its name,
// kit and contact, read with ctx, which reads as the system. A read that fails
// leaves that part out of the page and is logged; it never fails the page.
func (s *Store) pageBrandFor(ctx context.Context, logger *slog.Logger, accountID string, contact contactLookup) pageBrand {
	org, err := s.AccountName(ctx, accountID)
	if err != nil {
		logger.Warn("campaigns: an email page could not read its organization's name", "account", bare(accountID), "error", err)
		org = ""
	}
	return s.brandFor(ctx, logger, accountID, org, contact)
}

// preferencesLinkFor is the member's own preferences page, the way back from a
// page that says they are unsubscribed. Empty, and logged, when it cannot be
// built.
func preferencesLinkFor(cfg Config, logger *slog.Logger, p LinkPayload) string {
	link, err := PreferencesURL(cfg, LinkPayload{OwnerUserID: p.OwnerUserID, RecipientID: bare(p.RecipientID), CampaignID: bare(p.CampaignID), AccountID: p.AccountID, EmailDigest: p.EmailDigest})
	if err != nil {
		logger.Warn("campaigns: an email page could not build the link to the member's preferences", "error", err)
		return ""
	}
	return link
}
