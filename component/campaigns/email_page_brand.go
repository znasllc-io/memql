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
)

// email_page_brand.go -- how the pages behind email links look and who they
// name.
//
// A person reaches these pages from a link in an email, and they should look
// like it came from the organization that sent it: its name in the tab, its logo
// and its colour on the button, and somebody to write to when the page cannot
// help. All of it comes from the organization the SIGNED token names. A token
// that did not verify names nobody, so a forged link cannot choose which
// organization a page speaks for.
//
// # What a brand is trusted with
//
// Brand values are operator input shown on a public page, so each is checked
// where it is read: a colour only in the two hex forms, a logo only as an
// absolute https address, everything escaped. The page never fails for want of a
// brand: an opt-out page has to work whatever state the kit is in, so a kit that
// does not read renders the page plain and says so in the log.

// pageBrand is what one page borrows from its organization. Every field may be
// empty; the zero value is the plain page.
type pageBrand struct {
	// Org is the organization's name.
	Org string
	// Color is the primary colour, "#rgb" or "#rrggbb", used for the button.
	Color string
	// Logo is an absolute https address of the organization's logo.
	Logo string
	// Contact is the address of somebody who can help: the organization's own
	// default sender's Reply-To, else that sender's address. Empty when the
	// organization has no sender of its own.
	Contact string
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
	if b.Contact == "" {
		return "<p>You can reply to the email this link came from.</p>"
	}
	return "<p>You can write to " + html.EscapeString(b.Contact) + " for help.</p>"
}

// buttonText is the colour text takes on the primary colour, chosen by
// luminance so the button stays readable whatever the brand picked.
func buttonText(colour string) string {
	hex := strings.TrimPrefix(colour, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	var lum float64
	for i, weight := range []float64{0.2126, 0.7152, 0.0722} {
		v, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return "#fff"
		}
		c := float64(v) / 255
		if c <= 0.03928 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		lum += weight * c
	}
	if lum > 0.4 {
		return "#111"
	}
	return "#fff"
}

// logoSource is the logo address a brand kit's logo may be shown from, or "".
// Only an absolute https address qualifies: the page's policy lets images load
// from https and nowhere else.
func logoSource(src string) string {
	src = strings.TrimSpace(src)
	if !strings.HasPrefix(src, "https://") || len(src) <= len("https://") || strings.ContainsAny(src, " \t\r\n\"'<>") {
		return ""
	}
	return src
}

// brandFor reads the organization's brand for a page. ctx carries the token's
// owner. org is the organization's name, already read by the caller (empty when
// it did not resolve). A read that fails costs the page that part of its brand
// and is logged at warn; it never fails the page.
func (s *Store) brandFor(ctx context.Context, logger *slog.Logger, accountID, org string) pageBrand {
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
	senders, err := s.SenderIdentitiesForAccount(ctx, accountID)
	if err != nil {
		logger.Warn("campaigns: an email page could not read its organization's senders; the page names no contact", "account", bare(accountID), "error", err)
		return brand
	}
	brand.Contact = defaultContact(senders, accountID)
	return brand
}

// defaultContact is who a page tells people to write to: the Reply-To of the
// organization's own default sender, else that sender's address. The default is
// the first sender the organization set up that is still on. A sender of another
// organization, and the installation's own identity, are never offered: the
// list is read by organization, and a row that names another is skipped.
func defaultContact(senders []SenderIdentity, accountID string) string {
	// Newest first: walk from the end so the oldest active sender wins.
	for i := len(senders) - 1; i >= 0; i-- {
		s := senders[i]
		if s.Disabled() || !sameOrganization(s.AccountID, accountID) {
			continue
		}
		if contact := strings.TrimSpace(s.ReplyTo); contact != "" {
			return contact
		}
		if contact := strings.TrimSpace(s.Address); contact != "" {
			return contact
		}
	}
	return ""
}
