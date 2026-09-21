package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// shopper_handler.go -- THE BFF HALF OF THE SHOPPER SURFACE (epic
// memql#5532, issue memql#5551).
//
// # The header is a POINTER, never an assertion
//
// The edge stamps which site a request arrived on, which store is in effect
// and whose authority to borrow, having stripped any copy the client sent.
// That the route is classified servedButNotExternallyRouted -- no front
// door rule exists for it, so the edge is the only way in -- is DEFENCE IN
// DEPTH and deliberately not the only control, because a control whose
// whole strength is "nothing else should be able to call this" is one
// misrouted Ingress away from being no control at all.
//
// So this handler re-derives everything from the graph:
//
//  1. THE OWNER IS SELF-CHECKING. The site row is read UNDER the named
//     user's own actor, and v1:platform:site is composite-owner tier, so a
//     user who does not own that site reads ZERO ROWS. A forged owner
//     refuses itself, with no comparison written down to get wrong.
//  2. THE SURFACE MUST BE ON, on the row, not in the header.
//  3. THE STORE MUST BE ONE THE SITE IS BOUND TO -- binding.storeId or
//     previewBinding.storeId -- so a stamp cannot aim a row at somebody
//     else's store. A site with no binding must present no store, which is
//     the same rule read from the other end.
//
// The most a forged header can therefore achieve is a request that the
// merchant's own storefront could already make.
//
// # What a shopper sees
//
// A FORM ANSWERS 303, ALWAYS, and to a page the PACK DECLARED -- so a
// refusal renders in the merchant's own design saying what happened, rather
// than in ours. The reason rides ?reason=<code> on the declared error page.
// That is why so little HTML lives here: the edge renders a page only for
// the refusals it makes BEFORE a declaration is in hand (shopper_page.go
// there explains why it must), and everything this handler refuses has a
// declaration and therefore somewhere better to send a person.
//
// The one exception is a request naming a form that does not resolve at
// all: there is no declared page to redirect to, so it is a bare 404. A
// shopper cannot reach that by filling in a form -- it means the pack is
// disabled or the name is wrong, which is a deploy-time fact.
//
// A READ ANSWERS JSON, so its refusals are JSON and no page is involved.
//
// # Why this is HTTP at all
//
// A plain HTML form post is a form post or it is not one. The first
// product's wholesale form is method="post" so a federal tax identifier
// never reaches a query string, a browser history entry or an access log,
// and its own comment forbids downgrading it to a JS submit handler because
// the served policy is script-src 'self'. There is no gRPC form of that
// conversation, exactly as there is none for /unsubscribe or a tracking
// pixel: the other party dictates the wire, and here the other party is a
// browser with no JavaScript and no MemQL identity.

// shopperReasonParam carries the refusal code to the merchant's own error
// page. A CODE RATHER THAN A SENTENCE, because the sentence is the
// merchant's to write in their own voice and their own language.
const shopperReasonParam = "reason"

// The refusal codes a declared form can answer with. Stable strings: a
// storefront renders copy per code, so renaming one silently changes what a
// shopper reads.
const (
	shopperReasonInvalid     = "invalid"     // a declared field failed its own rule
	shopperReasonTooLarge    = "too_large"   // the body exceeded the cap
	shopperReasonUnavailable = "unavailable" // the surface, site or store did not check out
	shopperReasonFailed      = "failed"      // the write was refused downstream
)

// ShopperSiteReader is the narrow read the handler needs.
//
// An interface in this package's own terms rather than the engine's, for
// the reason every handler here takes one: the tests fake it, and
// component/server is a tiered module that cannot reach component/edge or
// the root module's types.
type ShopperSiteReader interface {
	// ShopperSite resolves a site row UNDER ownerUserId's own actor.
	// A miss is (nil, nil): a site the named user cannot read is not an
	// error, it is a request that gets what an unauthenticated one gets.
	ShopperSite(ctx context.Context, siteID, ownerUserID string) (*ShopperSite, error)
}

// ShopperSite is the projection this handler decides on.
//
// FOUR FIELDS, and the omissions are the point. There is no bundleRef, no
// settings and no title here: a handler that cannot see them cannot leak
// them into a public response by accident.
type ShopperSite struct {
	ID             string
	ShopperForms   bool
	StoreID        string
	PreviewStoreID string
}

// ShopperHandler serves a pack's declared forms and reads.
type ShopperHandler struct {
	engine MemQLExecutor
	sites  ShopperSiteReader
	logger *slog.Logger
}

func NewShopperHandler(engine MemQLExecutor, sites ShopperSiteReader, logger *slog.Logger) *ShopperHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ShopperHandler{engine: engine, sites: sites, logger: logger}
}

var _ http.Handler = (*ShopperHandler)(nil)

func (h *ShopperHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, memql.ShopperFormPathPrefix):
		h.serveForm(w, r)
	case strings.HasPrefix(r.URL.Path, memql.ShopperReadPathPrefix):
		h.serveRead(w, r)
	default:
		http.NotFound(w, r)
	}
}

// shopperStamp is what the edge said, before any of it is believed.
type shopperStamp struct {
	siteID  string
	storeID string
	owner   string
}

func readShopperStamp(r *http.Request) (shopperStamp, bool) {
	s := shopperStamp{
		siteID:  strings.TrimSpace(r.Header.Get(memql.ShopperSiteHeader)),
		storeID: strings.TrimSpace(r.Header.Get(memql.ShopperStoreHeader)),
		owner:   strings.TrimSpace(r.Header.Get(memql.ShopperOwnerHeader)),
	}
	// FAILS CLOSED WITH NO CREDENTIALS, which is exactly what membership in
	// HandlerAuthorizedPaths() certifies: a request carrying no stamp at all
	// -- the shape a direct caller would send -- reaches no read and no
	// write.
	return s, s.siteID != "" && s.owner != ""
}

// resolveSite re-derives the site from the graph and refuses every way the
// stamp can fail to check out.
func (h *ShopperHandler) resolveSite(ctx context.Context, stamp shopperStamp) *ShopperSite {
	if h.sites == nil {
		return nil
	}
	site, err := h.sites.ShopperSite(ctx, stamp.siteID, stamp.owner)
	if err != nil {
		h.logger.Warn("shopper surface: resolving the site failed",
			"component", "server", "site", stamp.siteID, "err", err)
		return nil
	}
	if site == nil || !site.ShopperForms {
		return nil
	}
	// THE STORE MUST BE ONE THIS SITE IS BOUND TO, and an unbound site must
	// present none. Both directions, because "empty passes" would let a
	// storefront's rows be written with no scope at all and "any value
	// passes" would let them be written under somebody else's store.
	if stamp.storeID != site.StoreID && stamp.storeID != site.PreviewStoreID {
		h.logger.Warn("shopper surface: refusing a store this site is not bound to",
			"component", "server", "site", site.ID)
		return nil
	}
	return site
}

// parseShopperRoute splits "{prefix}{pack}/{name}".
func parseShopperRoute(path, prefix string) (pack, name string, ok bool) {
	rest := strings.TrimPrefix(path, prefix)
	pack, name, found := strings.Cut(rest, "/")
	if !found || pack == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return pack, name, true
}

func (h *ShopperHandler) serveForm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	pack, name, ok := parseShopperRoute(r.URL.Path, memql.ShopperFormPathPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// THE DECLARATION IS RESOLVED FIRST, because everything after it can be
	// answered with a redirect to a page the declaration names, and a
	// refusal before it cannot.
	form := memql.ShopperFormFor(pack, name)
	if form == nil {
		// No declared page to send anyone to. A shopper cannot reach this by
		// filling in a form: it means the pack is disabled or the name is
		// wrong, both deploy-time facts.
		http.NotFound(w, r)
		return
	}

	stamp, stamped := readShopperStamp(r)
	if !stamped {
		http.NotFound(w, r)
		return
	}
	site := h.resolveSite(r.Context(), stamp)
	if site == nil {
		h.redirect(w, r, form.RedirectError, shopperReasonUnavailable)
		return
	}

	// THE REAL CAP, enforced while reading. The edge refused an honestly
	// declared oversize Content-Length; this refuses a dishonest one.
	r.Body = http.MaxBytesReader(w, r.Body, shopperMaxBodyBytes())
	if err := r.ParseForm(); err != nil {
		if strings.Contains(err.Error(), "too large") {
			h.redirect(w, r, form.RedirectError, shopperReasonTooLarge)
			return
		}
		h.redirect(w, r, form.RedirectError, shopperReasonInvalid)
		return
	}

	args, bad := shopperArgs(form.Fields, r.PostForm.Get)
	if bad != "" {
		h.redirect(w, r, form.RedirectError, shopperReasonInvalid)
		return
	}
	// STAMPED LAST so nothing a caller sent can reach these keys, whatever
	// the declaration said. The registry already refuses a field with one of
	// these names; this is the second half of that, at the only place it
	// could still go wrong.
	args["storeId"] = stamp.storeID
	args["siteId"] = site.ID

	call, err := dslCall(memql.ShopperCallPrefix(form.Kind)+form.Construct, args)
	if err != nil {
		h.logger.Error("shopper surface: could not render the construct call",
			"component", "server", "construct", form.Construct, "err", err)
		h.redirect(w, r, form.RedirectError, shopperReasonFailed)
		return
	}

	// BORROWED AUTHORITY: the merchant owns what is written through their
	// storefront, and the shopper is data on it. Never internal origin --
	// that would put every @serverOnly construct within reach of a public
	// form, which is the opposite of what a narrowly declared surface means.
	ctx := auth.ContextWithUserActor(r.Context(), stamp.owner)
	if _, err := h.engine.Execute(ctx, call); err != nil {
		h.logger.Warn("shopper surface: the write was refused",
			"component", "server", "pack", pack, "form", name,
			"site", site.ID, "err", err)
		h.redirect(w, r, form.RedirectError, shopperReasonFailed)
		return
	}
	h.redirect(w, r, form.RedirectOK, "")
}

func (h *ShopperHandler) serveRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	pack, name, ok := parseShopperRoute(r.URL.Path, memql.ShopperReadPathPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	read := memql.ShopperReadFor(pack, name)
	if read == nil {
		http.NotFound(w, r)
		return
	}
	stamp, stamped := readShopperStamp(r)
	if !stamped {
		http.NotFound(w, r)
		return
	}
	site := h.resolveSite(r.Context(), stamp)
	if site == nil {
		http.NotFound(w, r)
		return
	}

	args, bad := shopperArgs(read.Fields, r.URL.Query().Get)
	if bad != "" {
		writeShopperJSONError(w, http.StatusBadRequest, shopperReasonInvalid)
		return
	}
	args["storeId"] = stamp.storeID
	args["siteId"] = site.ID

	call, err := dslCall(memql.ShopperCallPrefix(read.Kind)+read.Construct, args)
	if err != nil {
		writeShopperJSONError(w, http.StatusInternalServerError, shopperReasonFailed)
		return
	}
	result, err := h.engine.Execute(auth.ContextWithUserActor(r.Context(), stamp.owner), call)
	if err != nil {
		h.logger.Warn("shopper surface: the read was refused",
			"component", "server", "pack", pack, "read", name, "site", site.ID, "err", err)
		writeShopperJSONError(w, http.StatusBadGateway, shopperReasonFailed)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A PUBLIC READ ON A MERCHANT'S ORIGIN. It is cacheable for a short
	// while because a storefront renders it on every product page, and it is
	// explicitly not private: nothing here is per-visitor, because there is
	// no visitor identity in the first place.
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": result})
}

// shopperArgs validates the declared fields against a value source and
// builds the construct's arguments.
//
// AN UNDECLARED INPUT IS DROPPED, NOT REFUSED, and the asymmetry is
// deliberate: a browser posts what the page contains, so a hidden CSRF
// token, an honeypot field or a stray input the merchant added would turn
// every submission into a server error if an unknown name were fatal. A
// DECLARED field that fails its own rule IS refused, because that is the
// merchant's own contract being broken and the shopper can fix it.
func shopperArgs(fields []memql.ShopperField, get func(string) string) (map[string]any, string) {
	args := make(map[string]any, len(fields))
	for _, f := range fields {
		raw := strings.TrimSpace(get(f.Name))
		if raw == "" {
			if f.Required {
				return nil, f.Name
			}
			continue
		}
		if len(raw) > f.EffectiveMaxLength() {
			return nil, f.Name
		}
		if len(f.Enum) > 0 && !containsString(f.Enum, raw) {
			return nil, f.Name
		}
		if f.Numeric {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, f.Name
			}
			args[f.Name] = n
			continue
		}
		args[f.Name] = raw
	}
	return args, ""
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// redirect answers the 303 a plain form post needs.
//
// 303 rather than 302, because the reply to a POST must be fetched with GET
// -- a 302 leaves the method to the browser's discretion and a re-POST on
// refresh would write the row twice.
func (h *ShopperHandler) redirect(w http.ResponseWriter, r *http.Request, target, reason string) {
	if reason != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + shopperReasonParam + "=" + reason
	}
	// The target came from the pack's declaration, which the registry
	// validated as site-relative at registration time. Nothing the request
	// sent reaches it.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func writeShopperJSONError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"error":%q}`, reason)))
}

// shopperMaxBodyBytes mirrors the edge's cap. Read from the same env var so
// an operator sets one number; the two checks answer different questions
// (an honest Content-Length, and the bytes actually sent).
func shopperMaxBodyBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("MEMQL_SHOPPER_FORM_MAX_BYTES"))
	if raw == "" {
		return 64 << 10
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 64 << 10
	}
	return n
}
