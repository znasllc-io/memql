package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// me_legal_acceptance.go -- the "Legal acceptance" card on /me/settings: the
// versions of the Terms of Service and the Privacy Notice THIS person
// accepted by signing in, read from their own row.
//
// The record is written by the session seam
// (component/identity/http.createSessionRow) at every sign-in that creates a
// session, once per document version; this is where a person sees it. MemQL
// OS draws the same data from the page's native representation
// (clients/os/src/apps/identity/IdentityAccount.tsx).

// legalDocumentTitles maps the stored documentType to the title each page
// shows. An unknown type is shown as stored rather than hidden: a record is
// not something to drop because the display has no name for it.
var legalDocumentTitles = map[string]string{
	"tos":     "Terms of Service",
	"privacy": "Privacy Notice",
}

// legalAcceptanceCard reads the caller's own history.
//
// A LISTING ERROR IS NOT AN EMPTY LIST, the rule the sessions panel follows:
// on failure Available is false and the card says it could not load, rather
// than an empty table that reads as "you have agreed to nothing". The source
// IP stays on the record and off the page -- the person's card lists what
// they accepted and when.
func (s *Server) legalAcceptanceCard(r *http.Request, claims *identity.AccessTokenClaims) webtempl.LegalAcceptanceData {
	out := webtempl.LegalAcceptanceData{Available: false}
	if s == nil || s.Store == nil || claims == nil {
		return out
	}
	entries, _, err := s.Store.LegalAcceptanceForUser(r.Context(), claims.Subject)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("me-settings: legal acceptance lookup failed", "error", err, "userId", claims.Subject)
		}
		return out
	}
	out.Available = true

	type dated struct {
		entry identity.LegalAcceptanceEntry
		at    time.Time
	}
	ordered := make([]dated, 0, len(entries))
	for _, e := range entries {
		at, _ := time.Parse(time.RFC3339Nano, e.AcceptedAt)
		ordered = append(ordered, dated{entry: e, at: at})
	}
	// Newest first, as the sessions table is: the version a person is
	// looking for is the one they accepted last. Stable, so the two entries
	// one sign-in writes keep the order they were written in, and an entry
	// whose instant does not parse sorts last rather than disappearing.
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].at.After(ordered[j].at)
	})

	out.Items = make([]webtempl.LegalAcceptanceRow, 0, len(ordered))
	for _, d := range ordered {
		title, ok := legalDocumentTitles[d.entry.DocumentType]
		if !ok {
			title = d.entry.DocumentType
		}
		shown := d.entry.AcceptedAt
		if !d.at.IsZero() {
			shown = formatSessionTime(d.at, time.Time{})
		}
		out.Items = append(out.Items, webtempl.LegalAcceptanceRow{
			DocumentType: d.entry.DocumentType,
			Document:     title,
			Version:      d.entry.Version,
			AcceptedAt:   shown,
		})
	}
	return out
}
