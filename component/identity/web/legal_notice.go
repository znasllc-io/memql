package web

import (
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
)

// legal_notice.go -- what THIS sign-in accepted, for the notice MemQL OS
// shows after sign-in.
//
// The Terms of Service and the Privacy Notice promise notice in the platform
// when a version changes. The sign-in page cannot give it: before a person
// signs in it does not know who they are. The moment it CAN be given is right
// after sign-in, and the fact to report is already on the record: the session
// seam appended an entry for every document whose version in force differed
// from what the person had accepted (or they had accepted nothing yet), and
// stamped the entry with the session it created. So the account overview
// (GET /me/, native) lists the entries that carry the caller's own session id,
// each with the document's title and effective date, and the OS says so once.
//
// Nothing is tracked about whether the person saw it: the OS keeps a
// per-tab dismissal, and a later session that recorded nothing gets an empty
// list. The list is ABSENT when the history could not be read, so the OS can
// tell "nothing to say" from "could not read" and never says the former on
// the strength of the latter.

// LegalNotice is one version this sign-in accepted, as the overview reports it.
type LegalNotice struct {
	// DocumentType is the stored discriminator, "tos" or "privacy".
	DocumentType string
	// Document is the title the pages show.
	Document string
	Version  string
	// EffectiveDate is the front matter's date when the recorded version is
	// the one this node serves; empty otherwise (the record outlived the
	// document, or an override moved).
	EffectiveDate string
	// Changed is true when the person had accepted an EARLIER version of the
	// document: the version changed under them. False for a first acceptance.
	Changed bool
}

// legalNoticesForSession picks the entries sessionId recorded, in array
// order, and decides Changed from the entries before each.
func (s *Server) legalNoticesForSession(entries []identity.LegalAcceptanceEntry, sessionId string) []LegalNotice {
	sessionId = strings.TrimSpace(sessionId)
	out := make([]LegalNotice, 0, 2)
	if sessionId == "" {
		return out
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.SessionId != sessionId {
			seen[e.DocumentType] = true
			continue
		}
		title, ok := legalDocumentTitles[e.DocumentType]
		if !ok {
			title = e.DocumentType
		}
		out = append(out, LegalNotice{
			DocumentType:  e.DocumentType,
			Document:      title,
			Version:       e.Version,
			EffectiveDate: s.effectiveDateFor(e.DocumentType, e.Version),
			Changed:       seen[e.DocumentType],
		})
		seen[e.DocumentType] = true
	}
	return out
}

// effectiveDateFor is the served document's effective date when version is
// the version served, else empty.
func (s *Server) effectiveDateFor(documentType, version string) string {
	if s == nil {
		return ""
	}
	var doc legalDocument
	switch documentType {
	case "tos":
		doc = s.tos
	case "privacy":
		doc = s.privacy
	default:
		return ""
	}
	if strings.TrimSpace(doc.Version) != strings.TrimSpace(version) {
		return ""
	}
	return doc.EffectiveDate
}

// legalNotices reads the caller's history and returns what their session
// recorded; nil when the history could not be read.
func (s *Server) legalNotices(r *http.Request, claims *identity.AccessTokenClaims) []LegalNotice {
	if s == nil || s.Store == nil || claims == nil {
		return nil
	}
	entries, _, err := s.Store.LegalAcceptanceForUser(r.Context(), claims.Subject)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("me-overview: legal acceptance lookup failed", "error", err, "userId", claims.Subject)
		}
		return nil
	}
	return s.legalNoticesForSession(entries, claims.SessionId)
}
